// deevnet-log-bridge carries edge devices' log messages from the MQTT broker
// into each tenant's partition of the log store (ADR-0027 §4).
//
// It runs beside the broker on the messaging VM, subscribes to every tenant's
// log topic, and posts each message to the store through the authenticating
// proxy. It holds one store token for every tenant; the tenant a batch belongs
// to travels in a header the proxy matches against routes the Deevnet API
// wrote, and then overwrites.
//
// What it is not:
//
//   - It is not in a device's path. Stopping it loses logs; it does not stop a
//     device working, and the broker goes on accepting messages either way.
//   - It never publishes. Its broker account has no publish grant.
//   - It never takes a tenant from a payload. The broker enforces each
//     account's topic prefix, so the first topic level is an identity that has
//     already been checked; a payload is content.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/deevnet/deevnet-log-bridge/internal/bridge"
	"github.com/deevnet/deevnet-log-bridge/internal/version"
)

type config struct {
	brokerURL  string
	username   string
	password   string
	clientID   string
	caFile     string
	topic      string
	storeURL   string
	storeToken string
	storeCA    string
	queueMax   int
	flush      time.Duration
	healthAddr string
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := loadConfig()
	if err != nil {
		log.Error("configuration", "err", err)
		os.Exit(1)
	}
	log.Info("starting", "version", version.Version, "commit", version.Commit,
		"broker", cfg.brokerURL, "topic", cfg.topic, "store", cfg.storeURL)

	shipper, err := bridge.NewShipper(cfg.storeURL, cfg.storeToken, cfg.storeCA, log)
	if err != nil {
		log.Error("the store", "err", err)
		os.Exit(1)
	}
	queue := bridge.NewQueue(shipper, cfg.queueMax, cfg.flush, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go queue.Run(ctx)
	go serveHealth(ctx, cfg.healthAddr, log)

	client, err := connect(cfg, queue, log)
	if err != nil {
		log.Error("the broker", "err", err)
		os.Exit(1)
	}

	<-ctx.Done()
	log.Info("stopping")
	// The queue's own Run flushes what it holds; this only stops more arriving.
	client.Disconnect(2000)
	// Give the final flush its moment.
	time.Sleep(time.Second)
}

// subackFailure is what a broker answers for a filter it will not grant.
// MQTT 3.1.1 §3.9.3: the return code 0x80 means failure, and it arrives in an
// otherwise successful SUBACK.
const subackFailure = 0x80

// subscribed is what /healthz reports. A bridge that is running but not
// subscribed is not doing its job, and the one thing this process must never
// do is look fine while carrying nothing.
var subscribed atomic.Bool

func connect(cfg config, queue *bridge.Queue, log *slog.Logger) (mqtt.Client, error) {
	opts := mqtt.NewClientOptions().
		AddBroker(cfg.brokerURL).
		SetClientID(cfg.clientID).
		SetUsername(cfg.username).
		SetPassword(cfg.password).
		// A PERSISTENT session, so the broker keeps this subscriber's QoS 1
		// messages while the bridge is restarting rather than dropping them on
		// the floor. What the broker will hold, and for how long, is its
		// setting and not this program's promise.
		SetCleanSession(false).
		SetOrderMatters(false).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetKeepAlive(30 * time.Second)

	if cfg.caFile != "" {
		pem, err := os.ReadFile(cfg.caFile)
		if err != nil {
			return nil, fmt.Errorf("reading the broker CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("the broker CA at %s holds no certificate", cfg.caFile)
		}
		opts.SetTLSConfig(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	}

	opts.SetOnConnectHandler(func(c mqtt.Client) {
		// Subscribed here rather than once after connecting: with a persistent
		// session a reconnect keeps the subscription, but a broker that lost
		// the session silently would leave this process connected and deaf.
		tok := c.Subscribe(cfg.topic, 1, func(_ mqtt.Client, m mqtt.Message) {
			line, err := bridge.Parse(m.Topic(), m.Payload(), time.Now())
			if err != nil {
				// A message on a topic this bridge cannot attribute is
				// dropped, and said out loud: it is either a device on the
				// wrong topic or a grant that should not exist.
				log.Warn("dropping a message", "topic", m.Topic(), "err", err)
				return
			}
			queue.Add(line)
		})
		if tok.Wait() && tok.Error() != nil {
			subscribed.Store(false)
			log.Error("subscribing", "topic", cfg.topic, "err", tok.Error())
			return
		}
		// A SUBACK is not an acceptance. A broker that refuses a filter on
		// ACL grounds answers 0x80 for it and leaves the connection up, and
		// the client library does not call that an error - so without this
		// the bridge would report itself subscribed, carry nothing, and look
		// healthy while doing it. Found by running this against an account
		// whose ACL did not cover the filter.
		if st, ok := tok.(*mqtt.SubscribeToken); ok {
			for filter, qos := range st.Result() {
				if qos == subackFailure {
					subscribed.Store(false)
					log.Error("the broker refused the subscription",
						"topic", filter,
						"hint", "the account's ACL does not cover this filter")
					return
				}
			}
		}
		subscribed.Store(true)
		log.Info("subscribed", "topic", cfg.topic)
	})
	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		subscribed.Store(false)
		log.Warn("lost the broker", "err", err)
	})

	c := mqtt.NewClient(opts)
	if tok := c.Connect(); tok.Wait() && tok.Error() != nil {
		return nil, tok.Error()
	}
	return c, nil
}

// serveHealth answers on loopback only.
//
// It reports one thing: whether this bridge holds the subscription it exists
// for. It does NOT report whether the store is reachable - that comes back by
// itself, and a bridge that cannot write for a minute is still the right
// process to leave running. A refused subscription is different: it does not
// come back by itself, because it means the account may not hold this filter.
func serveHealth(ctx context.Context, addr string, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !subscribed.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, "not subscribed")
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("health listener", "err", err)
	}
}

func loadConfig() (config, error) {
	cfg := config{
		brokerURL:  os.Getenv("DEEVNET_BRIDGE_BROKER_URL"),
		username:   os.Getenv("DEEVNET_BRIDGE_BROKER_USERNAME"),
		password:   os.Getenv("DEEVNET_BRIDGE_BROKER_PASSWORD"),
		clientID:   envOr("DEEVNET_BRIDGE_CLIENT_ID", "deevnet-log-bridge"),
		caFile:     os.Getenv("DEEVNET_BRIDGE_BROKER_CA_FILE"),
		topic:      envOr("DEEVNET_BRIDGE_TOPIC", "+/log/#"),
		storeURL:   os.Getenv("DEEVNET_BRIDGE_STORE_URL"),
		storeToken: os.Getenv("DEEVNET_BRIDGE_STORE_TOKEN"),
		storeCA:    os.Getenv("DEEVNET_BRIDGE_STORE_CA_FILE"),
		healthAddr: envOr("DEEVNET_BRIDGE_HEALTH_ADDR", "127.0.0.1:9099"),
	}
	var err error
	if cfg.queueMax, err = intEnv("DEEVNET_BRIDGE_QUEUE_MAX", 10000); err != nil {
		return config{}, err
	}
	if cfg.flush, err = durationEnv("DEEVNET_BRIDGE_FLUSH_INTERVAL", 2*time.Second); err != nil {
		return config{}, err
	}
	var missing []string
	for name, v := range map[string]string{
		"DEEVNET_BRIDGE_BROKER_URL":      cfg.brokerURL,
		"DEEVNET_BRIDGE_BROKER_USERNAME": cfg.username,
		"DEEVNET_BRIDGE_BROKER_PASSWORD": cfg.password,
		"DEEVNET_BRIDGE_STORE_URL":       cfg.storeURL,
		"DEEVNET_BRIDGE_STORE_TOKEN":     cfg.storeToken,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return config{}, fmt.Errorf("these are empty: %v", missing)
	}
	return cfg, nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func intEnv(name string, fallback int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive whole number", name)
	}
	return n, nil
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration, for example 2s", name)
	}
	return d, nil
}
