// Copyright (c) 2021, Cisco Systems, Inc.
// All rights reserved.

package pubsub

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"path"
	"sync"
	"time"

	"github.com/cisco-pxgrid/cloud-sdk-go/internal/rpc"
	"github.com/cisco-pxgrid/cloud-sdk-go/log"
)

// SubscriptionCallback is the callback that's invoked when a message/error is received for the
// subscription request.
//
// err is set to a non-nil error in case of an error
// id represents the message id
// headers are the headers associated with the message
// payload contains the message payload
type SubscriptionCallback func(err error, id string, headers map[string]string, payload []byte)

// Subscribe subscribes to a DxHub Pubsub Stream
func (c *Connection) Subscribe(stream string, handler SubscriptionCallback) error {
	c.subs.Lock()
	defer c.subs.Unlock()

	var sub *subscription
	if _, ok := c.subs.table[stream]; ok {
		return fmt.Errorf("Subscription for stream %s already exists", stream)
	}

	id, err := c.createSubscription(stream)
	if err != nil {
		return fmt.Errorf("Failed to create subscription for %s: %v", stream, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sub = &subscription{
		id:        id,
		stream:    stream,
		callback:  handler,
		ctx:       ctx,
		ctxCancel: cancel,
	}
	c.subs.table[stream] = sub

	c.wg.Add(1)
	sub.wg.Add(1)
	go c.subscriber(sub)

	return nil
}

// Unsubscribe unsubscribes from a DxHub Pubsub Stream
func (c *Connection) Unsubscribe(stream string) error {
	log.Logger.Debugf("Unsubscribing from DxHub Pubsub Stream %s", stream)
	c.subs.Lock()
	defer c.subs.Unlock()

	return c.unsubscribe(stream)
}

func (c *Connection) unsubscribe(stream string) error {
	sub, ok := c.subs.table[stream]
	if !ok {
		return fmt.Errorf("Subscription for stream %s doesn't exist", stream)
	}
	err := c.deleteSubscription(sub.id)
	if err != nil {
		return fmt.Errorf("Failed to unsubscribe from stream %s: %v", stream, err)
	}

	delete(c.subs.table, stream)
	sub.ctxCancel()
	sub.wg.Wait()
	log.Logger.Debugf("Successfully unsubscribed from stream %s", stream)
	return nil
}

func (c *Connection) sendConsumeMessage(subscriptionId, consumeCtx string) (<-chan *rpc.Response, error) {
	req, err := rpc.NewConsumeRequest(subscriptionId, consumeCtx)
	if err != nil {
		return nil, err
	}
	respCh := make(chan *rpc.Response, 1) // we expect 1 response back
	err = c.sendMessage(req, func(resp *rpc.Response) {
		respCh <- resp
		close(respCh)
	})
	if err != nil {
		return nil, err
	}
	return respCh, err
}

type subscription struct {
	stream    string
	id        string
	callback  SubscriptionCallback
	ctx       context.Context
	ctxCancel context.CancelFunc
	wg        sync.WaitGroup
}

// subscriber goroutine is spawned for each subscription to a stream
func (c *Connection) subscriber(sub *subscription) {
	consumeResponseTimeout := 5 * time.Second
	defer sub.wg.Done()
	defer c.wg.Done()
	log.Logger.Debugf("Starting subscriber thread for %s", sub.stream)

	consumeCtx := ""
loop:
	for {
		// send consume message for requesting data from the server
		respCh, err := c.sendConsumeMessage(sub.id, consumeCtx)
		if err != nil {
			log.Logger.Errorf("Failed to start consumption for stream %s: %v", sub.stream, err)
			sub.callback(err, "", nil, nil)
		} else {
			select {
			case resp := <-respCh:
				// received consume response from the processor
				if resp.Error.Code != 0 {
					log.Logger.Errorf("Consume error for stream %s: %v", sub.stream, resp.Error)
					sub.callback(fmt.Errorf("consume error: %v", resp.Error), resp.ID, nil, nil)
					break
				}
				res, err := resp.ConsumeResult()
				if err != nil {
					log.Logger.Errorf("Consume error for stream %s: %v", sub.stream, err)
					sub.callback(fmt.Errorf("consume error: %v", err), resp.ID, nil, nil)
					break
				}
				consumeCtx = res.ConsumeContext
				for stream, messages := range res.Messages {
					if stream != sub.stream {
						log.Logger.Errorf("Received consume message for stream %s, was expecting messages for stream %s", stream, sub.stream)
						continue
					}
					for _, m := range messages {
						payload, err := base64.StdEncoding.DecodeString(m.Payload)
						sub.callback(err, m.MsgID, m.Headers, payload)
					}
				}
			case <-time.After(consumeResponseTimeout):
				// Do not wait indefinitely for the consume response
				log.Logger.Debugf("Did not receive consume response within timeout, trying again...")
				break
			case <-sub.ctx.Done():
				// user unsubscribed from the stream
				break loop
			}
		}
		select {
		case <-sub.ctx.Done():
			// user unsubscribed from the stream
			break loop
		case <-time.After(c.config.PollInterval):
		}
	}
	log.Logger.Debugf("Stopped subscriber thread for %s", sub.stream)
}

type subscriptionReq struct {
	GroupID string   `json:"groupId"`
	Streams []string `json:"streams"`
}

type subscriptionResp struct {
	ID string `json:"_id"`
}

func (c *Connection) createSubscription(stream string) (string, error) {
	subReq := subscriptionReq{
		GroupID: c.config.GroupID,
		Streams: []string{stream},
	}
	subResp := subscriptionResp{}
	u := url.URL{
		Scheme: httpScheme,
		Host:   c.config.Domain,
		Path:   apiPaths.subscriptions,
	}
	authValue, err := c.authHeader.provider()
	if err != nil {
		log.Logger.Errorf("Failed to obtain auth header: %v", err)
		return "", err
	}
	resp, err := c.restClient.R().
		SetHeader(c.authHeader.key, string(authValue)).
		SetBody(subReq).
		SetResult(&subResp).
		Post(u.String())
	if err != nil {
		return "", fmt.Errorf("Failed to create subscription for %s: %v", stream, err)
	}

	if resp.StatusCode() < 200 || resp.StatusCode() >= 300 {
		log.Logger.Errorf("Received unexpected response '%s' while creating the subscription", resp.Status())
		return "", fmt.Errorf("Received unexpected response '%s' while creating the subscription", resp.Status())
	}

	log.Logger.Debugf("Subscription created: %+v", subResp)
	if subResp.ID == "" {
		return "", fmt.Errorf("Received empty subscriptions ID")
	}

	return subResp.ID, nil
}

func (c *Connection) deleteSubscription(id string) error {
	log.Logger.Debugf("Deleting subscription '%s'", id)
	u := url.URL{
		Scheme: httpScheme,
		Host:   c.config.Domain,
		Path:   path.Join(apiPaths.subscriptions, id),
	}

	token, err := c.authHeader.provider()
	if err != nil {
		return fmt.Errorf("Failed to obtain auth header for subscription %s deletion: %v", id, err)
	}
	resp, err := c.restClient.R().
		SetHeader(c.authHeader.key, string(token)).
		Delete(u.String())
	if err != nil || (resp.StatusCode() < 200 && resp.StatusCode() >= 300) {
		return fmt.Errorf("Failed to delete subscription: %v", err)
	}

	return nil
}
