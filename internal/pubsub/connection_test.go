package pubsub

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cisco-pxgrid/cloud-sdk-go/internal/pubsub/test"
	"github.com/stretchr/testify/require"
)

func Test_E2E(t *testing.T) {
	s := test.NewRPCServer(t, test.Config{
		PubSubPath:        apiPaths.pubsub,
		SubscriptionsPath: apiPaths.subscriptions,
	})
	defer s.Close()

	u, _ := url.Parse(s.URL)

	c, err := newInternalConnection(Config{
		GroupID: "test-client",
		Domain:  u.Host,
		APIKeyProvider: func() ([]byte, error) {
			return []byte("xyz"), nil
		},
		PollInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	require.NotNil(t, c)

	c.restClient.SetTLSClientConfig(&tls.Config{
		InsecureSkipVerify: true, // no verification for test server
	})

	err = c.connect(context.Background())
	require.NoError(t, err)

	// 5 subscriptions
	numSubs := 5
	receivedMsgs := make(map[string]int, numSubs)
	receivedMu := sync.Mutex{}
	for i := 0; i < numSubs; i++ {
		stream := fmt.Sprintf("test-stream-%d", i)
		receivedMu.Lock()
		receivedMsgs[stream] = 0
		receivedMu.Unlock()
		_, err = c.subscribe(stream, "",
			func(e error, id string, _ map[string]string, payload []byte) {
				t.Logf("Received message: %s, payload: %s", id, payload)
				receivedMu.Lock()
				receivedMsgs[stream]++
				receivedMu.Unlock()
				require.NoError(t, e)
			})
		require.NoError(t, err)
	}

	// publish to 5 streams simultaneously
	var wg sync.WaitGroup
	numMessages := 100
	for i := 0; i < numSubs; i++ {
		stream := fmt.Sprintf("test-stream-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < numMessages; i++ {
				payload := []byte("This is a test message on " + stream + ": " + strconv.Itoa(i))
				ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
				r, err := c.Publish(ctx, stream, nil, payload)
				cancel()
				require.NoError(t, err)
				t.Logf("Published message: %s to stream: %s, payload: %s", r.ID, stream, payload)
			}
		}()
	}
	wg.Wait()

	// wait for sever to respond to all consume requests
	time.Sleep(1 * time.Second)

	// verify
	for i := 0; i < numSubs; i++ {
		stream := fmt.Sprintf("test-stream-%d", i)
		receivedMu.Lock()
		require.Equal(t, numMessages, receivedMsgs[stream], "Did not receive all the mssages from stream", i)
		receivedMu.Unlock()
	}

	c.disconnect()
	require.Equal(t, true, c.isDisconnected(), "Connection is still connected")

	t.Logf("subs table: %#v", c.subs.table)
	require.Zero(t, len(c.subs.table))

	select {
	case err = <-c.Error:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatalf("Timed out waiting for the connection to close")
	}
}

func Test_ConnectionError(t *testing.T) {
	s := test.NewRPCServer(t, test.Config{
		PubSubPath:        apiPaths.pubsub,
		SubscriptionsPath: apiPaths.subscriptions,
		RejectConn:        true,
	})
	defer s.Close()

	u, _ := url.Parse(s.URL)

	c, err := newInternalConnection(Config{
		GroupID: "test-client",
		Domain:  u.Host,
		APIKeyProvider: func() ([]byte, error) {
			return []byte("xyz"), nil
		},
		PollInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	require.NotNil(t, c)

	c.restClient.SetTLSClientConfig(&tls.Config{
		InsecureSkipVerify: true, // no verification for test server
	})

	err = c.connect(context.Background())
	require.Error(t, err, "Did not receive expected error")
}

func Test_ConnectionMissingAppName(t *testing.T) {
	c, err := newInternalConnection(Config{
		Domain: "example.com", // doesn't matter for this case
		APIKeyProvider: func() ([]byte, error) {
			return []byte("xyz"), nil
		},
	})
	require.Error(t, err, "Did not receive expected error")
	require.Equal(t, "Config must contain GroupID", err.Error())
	require.Nil(t, c, "Connection is not nil")
}

func Test_ConnectionMissingDomain(t *testing.T) {
	c, err := newInternalConnection(Config{
		GroupID: "test-app",
		APIKeyProvider: func() ([]byte, error) {
			return []byte("xyz"), nil
		},
	})
	require.Error(t, err, "Did not receive expected error")
	require.Equal(t, "Config must contain Domain", err.Error())
	require.Nil(t, c, "Connection is not nil")
}

func Test_ConnectionMissingAuthProvider(t *testing.T) {
	c, err := newInternalConnection(Config{
		GroupID: "test-app",
		Domain:  "example.com", // doesn't matter for this case
	})
	require.Error(t, err, "Did not receive expected error")
	require.Equal(t, "Config must contain either APIKeyProvider or AuthTokenProvider", err.Error())
	require.Nil(t, c, "Connection is not nil")
}

func Test_AuthProviders(t *testing.T) {
	t.Run("ApiKeyTest", func(t *testing.T) {
		c, err := newInternalConnection(Config{
			GroupID: "test-app",
			Domain:  "example.com", // doesn't matter for this case
			APIKeyProvider: func() ([]byte, error) {
				return []byte("xyz"), nil
			},
		})

		require.NoError(t, err)
		require.NotNil(t, c)

		require.Equal(t, "X-Api-Key", c.authHeader.key)
		authHeader, _ := c.authHeader.provider()
		require.Equal(t, []byte("xyz"), authHeader)
	})

	t.Run("AuthTokenTest", func(t *testing.T) {
		c, err := newInternalConnection(Config{
			GroupID: "test-app",
			Domain:  "example.com", // doesn't matter for this case
			AuthTokenProvider: func() ([]byte, error) {
				return []byte("abc"), nil
			},
		})
		require.NoError(t, err)
		require.NotNil(t, c)

		require.Equal(t, "X-Auth-Token", c.authHeader.key)
		authHeader, _ := c.authHeader.provider()
		require.Equal(t, []byte("abc"), authHeader)
	})
}

func Test_ConnectAlreadyConnected(t *testing.T) {
	s := test.NewRPCServer(t, test.Config{
		PubSubPath:        apiPaths.pubsub,
		SubscriptionsPath: apiPaths.subscriptions,
	})
	defer s.Close()

	u, _ := url.Parse(s.URL)

	c, err := newInternalConnection(Config{
		GroupID: "test-client",
		Domain:  u.Host,
		APIKeyProvider: func() ([]byte, error) {
			return []byte("xyz"), nil
		},
		PollInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	require.NotNil(t, c)

	c.restClient.SetTLSClientConfig(&tls.Config{
		InsecureSkipVerify: true, // no verification for test server
	})

	err = c.connect(context.Background())
	require.NoError(t, err)
	defer c.disconnect()

	err = c.connect(context.Background())
	require.Error(t, err, "Did not receive expected error")
}

func Test_ConnectAuthTokenError(t *testing.T) {
	c, err := newInternalConnection(Config{
		GroupID: "test-client",
		Domain:  "example.com",
		APIKeyProvider: func() ([]byte, error) {
			return nil, fmt.Errorf("Error")
		},
		PollInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	require.NotNil(t, c)

	err = c.connect(context.Background())
	require.Error(t, err, "Did not receive expected error")
}

func Test_ConsumeError(t *testing.T) {
	s := test.NewRPCServer(t, test.Config{
		PubSubPath:        apiPaths.pubsub,
		SubscriptionsPath: apiPaths.subscriptions,
		ConsumeError:      true,
	})
	defer s.Close()

	u, _ := url.Parse(s.URL)

	c, err := newInternalConnection(Config{
		GroupID: "test-client",
		Domain:  u.Host,
		APIKeyProvider: func() ([]byte, error) {
			return []byte("xyz"), nil
		},
	})
	require.NoError(t, err)
	require.NotNil(t, c)

	c.restClient.SetTLSClientConfig(&tls.Config{
		InsecureSkipVerify: true, // no verification for test server
	})

	err = c.connect(context.Background())
	require.NoError(t, err)

	count := 0
	_, err = c.subscribe("test-stream", "",
		func(e error, _ string, _ map[string]string, _ []byte) {
			t.Logf("Got error: %v", e)
			require.Error(t, e)
			if e == nil {
				count++
			}
		})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err = c.Publish(ctx, "test-stream", nil, []byte("test payload"))
	require.NoError(t, err)
	time.Sleep(2 * time.Second)

	c.disconnect()

	require.True(t, c.isDisconnected())
	require.Zero(t, len(c.subs.table))
	require.Zero(t, count)
}

func Test_PublishError1(t *testing.T) {
	c, err := newInternalConnection(Config{
		GroupID: "test-client",
		Domain:  "example.com",
		APIKeyProvider: func() ([]byte, error) {
			return []byte("xyz"), nil
		},
	})
	require.NoError(t, err)
	require.NotNil(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err = c.Publish(ctx, "test-stream", nil, []byte("test payload"))
	require.Error(t, err, "Did not receive expected error")
}

func Test_PublishError2(t *testing.T) {
	s := test.NewRPCServer(t, test.Config{
		PubSubPath:        apiPaths.pubsub,
		SubscriptionsPath: apiPaths.subscriptions,
		PublishError:      true,
	})
	defer s.Close()

	u, _ := url.Parse(s.URL)

	c, err := newInternalConnection(Config{
		GroupID: "test-client",
		Domain:  u.Host,
		APIKeyProvider: func() ([]byte, error) {
			return []byte("xyz"), nil
		},
	})
	require.NoError(t, err)
	require.NotNil(t, c)

	c.restClient.SetTLSClientConfig(&tls.Config{
		InsecureSkipVerify: true, // no verification for test server
	})

	err = c.connect(context.Background())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r, err := c.Publish(ctx, "test-stream", nil, []byte("test payload"))
	require.NoError(t, err)
	require.Error(t, r.Error)

	c.disconnect()

	require.True(t, c.isDisconnected())
	require.Zero(t, len(c.subs.table))
}

func Test_PublishAsync(t *testing.T) {
	s := test.NewRPCServer(t, test.Config{
		PubSubPath:        apiPaths.pubsub,
		SubscriptionsPath: apiPaths.subscriptions,
	})
	defer s.Close()

	u, _ := url.Parse(s.URL)

	c, err := newInternalConnection(Config{
		GroupID: "test-client",
		Domain:  u.Host,
		APIKeyProvider: func() ([]byte, error) {
			return []byte("xyz"), nil
		},
	})
	require.NoError(t, err)
	require.NotNil(t, c)

	c.restClient.SetTLSClientConfig(&tls.Config{
		InsecureSkipVerify: true, // no verification for test server
	})

	err = c.connect(context.Background())
	require.NoError(t, err)

	subCh := make(chan []byte)
	_, err = c.subscribe("test-stream", "",
		func(e error, id string, _ map[string]string, payload []byte) {
			require.NoError(t, e)
			t.Logf("Received message %s: %s", id, payload)
			subCh <- payload
		})
	require.NoError(t, err)

	ack := make(chan *PublishResult)
	id, cancel, err := c.PublishAsync("test-stream", nil, []byte("test payload"), ack)
	require.NoError(t, err)

	select {
	case r := <-ack:
		require.Equal(t, id, r.ID)
		require.NoError(t, r.Error)
	case <-time.After(time.Second):
		require.FailNow(t, "Publish timed out")
	}
	cancel()

	select {
	case payload := <-subCh:
		require.Equal(t, payload, []byte("test payload"))
	case <-time.After(time.Second):
		require.FailNow(t, "Consume timed out")
	}

	c.disconnect()

	require.True(t, c.isDisconnected())
	require.Zero(t, len(c.subs.table))
}

func Test_PublishAsyncCanceled(t *testing.T) {
	s := test.NewRPCServer(t, test.Config{
		PubSubPath:        apiPaths.pubsub,
		SubscriptionsPath: apiPaths.subscriptions,
	})
	defer s.Close()

	u, _ := url.Parse(s.URL)

	c, err := newInternalConnection(Config{
		GroupID: "test-client",
		Domain:  u.Host,
		APIKeyProvider: func() ([]byte, error) {
			return []byte("xyz"), nil
		},
	})
	require.NoError(t, err)
	require.NotNil(t, c)

	c.restClient.SetTLSClientConfig(&tls.Config{
		InsecureSkipVerify: true, // no verification for test server
	})

	err = c.connect(context.Background())
	require.NoError(t, err)

	count := 0
	_, err = c.subscribe("test-stream", "",
		func(e error, id string, _ map[string]string, payload []byte) {
			require.NoError(t, e)
			t.Logf("Received message %s: %s", id, payload)
			count++
		})
	require.NoError(t, err)

	ack := make(chan *PublishResult)
	_, cancel, err := c.PublishAsync("test-stream", nil, []byte("test payload"), ack)
	require.NoError(t, err)
	cancel()

	select {
	case <-ack:
		require.Fail(t, "did not expect ack")
	case <-time.After(time.Second):
	}

	c.disconnect()

	require.True(t, c.isDisconnected())
	require.Zero(t, len(c.subs.table))
	require.Equal(t, 1, count)
}

func Test_ConsumeTimeout(t *testing.T) {
	s := test.NewRPCServer(t, test.Config{
		PubSubPath:        apiPaths.pubsub,
		SubscriptionsPath: apiPaths.subscriptions,
		ConsumeDrop:       true,
	})
	defer s.Close()

	// Change to shorter timeout
	consumeResponseTimeout = 2 * time.Second

	u, _ := url.Parse(s.URL)

	c, err := newInternalConnection(Config{
		GroupID: "test-client",
		Domain:  u.Host,
		APIKeyProvider: func() ([]byte, error) {
			return []byte("xyz"), nil
		},
	})
	require.NoError(t, err)
	require.NotNil(t, c)

	c.restClient.SetTLSClientConfig(&tls.Config{
		InsecureSkipVerify: true, // no verification for test server
	})

	err = c.connect(context.Background())
	require.NoError(t, err)

	_, err = c.subscribe("test-stream", "",
		func(_ error, _ string, _ map[string]string, _ []byte) {
			require.Fail(t, "Unexpected message")
		})
	require.NoError(t, err)

	select {
	case <-c.Error:
		require.True(t, c.consumeTimeout)
	case <-time.After(5 * time.Second):
		require.Fail(t, "Error expected")
	}
	c.disconnect()
}
