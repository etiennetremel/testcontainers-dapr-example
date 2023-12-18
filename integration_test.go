package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"testing"

	"github.com/dapr/go-sdk/service/common"
	daprd "github.com/dapr/go-sdk/service/http"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

var sub = &common.Subscription{
	PubsubName: "order-pub-sub",
	Topic:      "orders",
	Route:      "/checkout",
}

type appContainer struct {
	testcontainers.Container
	URI string
}

type containers struct {
	app             *appContainer
	daprApp         testcontainers.Container
	daprIntegration testcontainers.Container
	redis           testcontainers.Container
}

// helper to display container logs
func showContainerLogs(ctx context.Context, c testcontainers.Container) error {
	name, err := c.Name(ctx)
	if err != nil {
		log.Fatal(err)
	}
	buf := new(bytes.Buffer)
	readCloser, err := c.Logs(ctx)
	if err != nil {
		log.Fatal(err)
	}
	_, err = buf.ReadFrom(readCloser)
	if err != nil {
		log.Fatal(err)
	}
	readCloser.Close()
	content := buf.String()
	fmt.Printf("[%s] container logs: %s\r\n", name, content)

	return nil
}

func setupApp(ctx context.Context) (*containers, error) {
	// Redis
	redisC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Name:         "redis",
			Hostname:     "redis",
			Image:        "redis:alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections tcp"),
			LifecycleHooks: []testcontainers.ContainerLifecycleHooks{
				{
					PreTerminates: []testcontainers.ContainerHook{
						showContainerLogs,
					},
				},
			},
		},
		Started: true,
	})
	if err != nil {
		return nil, err
	}

	appC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Name:         "app",
			Hostname:     "app",
			ExposedPorts: []string{"3000/tcp"},
			WaitingFor:   wait.ForHTTP("/health"),
			Env: map[string]string{
				"DAPR_URL": "dapr-app:50001",
			},
			FromDockerfile: testcontainers.FromDockerfile{
				Context:    ".",
				Dockerfile: "Dockerfile",
				KeepImage:  true,
			},
			LifecycleHooks: []testcontainers.ContainerLifecycleHooks{
				{
					PreTerminates: []testcontainers.ContainerHook{
						showContainerLogs,
					},
				},
			},
		},
		Started: true,
	})
	if err != nil {
		return nil, err
	}

	ip, err := appC.Host(ctx)
	if err != nil {
		return nil, err
	}

	mappedPort, err := appC.MappedPort(ctx, "3000")
	if err != nil {
		return nil, err
	}

	uri := fmt.Sprintf("http://%s:%s", ip, mappedPort.Port())

	// DAPR
	daprAppC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Name:         "dapr-app",
			Hostname:     "dapr-app",
			Image:        "daprio/daprd",
			WaitingFor:   wait.ForLog("dapr initialized"),
			ExposedPorts: []string{"3500/tcp", "50001/tcp"},
			Cmd: []string{
				"./daprd",
				"-app-id", "app",
				"-app-port", "3000",
				"-app-protocol", "http",
				"-app-channel-address", "app",
				"-dapr-listen-addresses", "0.0.0.0",
				"-resources-path", "./components",
				"-log-level", "debug",
			},
			Files: []testcontainers.ContainerFile{
				{
					HostFilePath:      "./order-pub-sub.yaml",
					ContainerFilePath: "./components/order-pub-sub.yaml",
					FileMode:          0o644,
				},
			},
			LifecycleHooks: []testcontainers.ContainerLifecycleHooks{
				{
					PreTerminates: []testcontainers.ContainerHook{
						showContainerLogs,
					},
				},
			},
		},
		Started: true,
	})
	if err != nil {
		return nil, err
	}

	// DAPR Integration
	daprIntegrationC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Name:         "dapr-integration",
			Hostname:     "dapr-integration",
			Image:        "daprio/daprd",
			WaitingFor:   wait.ForLog("dapr initialized"),
			ExposedPorts: []string{"3500/tcp", "50001/tcp"}, // HTTP + GRPC port
			Cmd: []string{
				"./daprd",
				"-app-id", "integration",
				"-app-port", "6002",
				"-app-protocol", "http",
				"-app-channel-address", "host.docker.internal",
				"-dapr-listen-addresses", "0.0.0.0",
				"-resources-path", "./components",
				"-log-level", "debug",
			},
			Files: []testcontainers.ContainerFile{
				{
					HostFilePath:      "./order-pub-sub.yaml",
					ContainerFilePath: "./components/order-pub-sub.yaml",
					FileMode:          0o644,
				},
			},
			LifecycleHooks: []testcontainers.ContainerLifecycleHooks{
				{
					PreTerminates: []testcontainers.ContainerHook{
						showContainerLogs,
					},
				},
			},
		},
		Started: true,
	})
	if err != nil {
		return nil, err
	}

	return &containers{
		app:             &appContainer{Container: appC, URI: uri},
		daprApp:         daprAppC,
		daprIntegration: daprIntegrationC,
		redis:           redisC,
	}, nil
}

func TestIntegrationPutOrderStatus(t *testing.T) {
	ctx := context.Background()
	receivedEvent := make(chan bool)

	// start integration server to check events
	go func() {
		s := daprd.NewService(":6002")
		log.Println("Running service at :6002")
		err := s.AddTopicEventHandler(sub, func(ctx context.Context, e *common.TopicEvent) (retry bool, err error) {
			log.Printf("Subscriber received: %s\n", e.RawData)

			// when event is received, we forward true to the channel
			defer func() {
				receivedEvent <- true
			}()

			var order Order
			if err := e.Struct(&order); err != nil {
				t.Fatalf("couldn't parse received event. Got %s. Err: %s", e.RawData, err)
			}

			if order.ID != "order-1234" || order.Status != OrderStatusPaid {
				t.Fatalf("expected event order id=order-1234, status=paid. Got %v.", order)
			}

			return false, nil
		})
		if err != nil {
			log.Fatalf("error adding topic subscription: %v", err)
		}

		if err := s.Start(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("error listening: %v", err)
		}
	}()

	// start containers
	runningContainers, err := setupApp(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// clean up the container after the test is complete
	t.Cleanup(func() {
		if err := runningContainers.daprIntegration.Terminate(ctx); err != nil {
			t.Fatalf("failed to terminate container: %s", err)
		}
		if err := runningContainers.daprApp.Terminate(ctx); err != nil {
			t.Fatalf("failed to terminate container: %s", err)
		}
		if err := runningContainers.app.Terminate(ctx); err != nil {
			t.Fatalf("failed to terminate container: %s", err)
		}
		if err := runningContainers.redis.Terminate(ctx); err != nil {
			t.Fatalf("failed to terminate container: %s", err)
		}
	})

	// make request to the app container
	url := fmt.Sprintf("%s/orders/order-1234", runningContainers.app.URI)
	payload := []byte(`{"status": "PAID"}`)

	req, err := http.NewRequest(http.MethodPut, url, bytes.NewBuffer(payload))
	if err != nil {
		t.Fatalf("couldn't create PUT request: %q", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("couldn't do request: %q", err)
	}

	// check response status
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status code %d. Got %d.", http.StatusOK, resp.StatusCode)
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if string(body) != "Order updated" {
		t.Fatalf("expected body \"Order updated\". Got %s.", body)
	}

	log.Println("Waiting for event to be published in orders topic")
	ok := <-receivedEvent
	log.Printf("Event received: %t\n", ok)
}
