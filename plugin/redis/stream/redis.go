package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/micro/micro/v3/service/events"
	"github.com/micro/micro/v3/service/logger"
	"github.com/pkg/errors"
)

type redisStream struct {
	sync.RWMutex
	redisClient *redis.Client
	attempts    map[string]int
}

func NewStream(opts ...Option) (events.Stream, error) {
	options := Options{}
	for _, o := range opts {
		o(&options)
	}
	rc := redis.NewClient(&redis.Options{
		Addr:      options.Address,
		Username:  options.User,
		Password:  options.Password,
		TLSConfig: options.TLSConfig,
	})
	rs := &redisStream{
		redisClient: rc,
		attempts:    map[string]int{},
	}
	return rs, nil
}

func (r *redisStream) Publish(topic string, msg interface{}, opts ...events.PublishOption) error {
	// validate the topic
	if len(topic) == 0 {
		return events.ErrMissingTopic
	}
	options := events.PublishOptions{
		Timestamp: time.Now(),
	}
	for _, o := range opts {
		o(&options)
	}

	// encode the message if it's not already encoded
	var payload []byte
	if p, ok := msg.([]byte); ok {
		payload = p
	} else {
		p, err := json.Marshal(msg)
		if err != nil {
			return events.ErrEncodingMessage
		}
		payload = p
	}

	// construct the event
	event := &events.Event{
		ID:        uuid.New().String(),
		Topic:     topic,
		Timestamp: options.Timestamp,
		Metadata:  options.Metadata,
		Payload:   payload,
	}

	// serialize the event to bytes
	bytes, err := json.Marshal(event)
	if err != nil {
		return errors.Wrap(err, "Error encoding event")
	}

	return r.redisClient.XAdd(context.Background(), &redis.XAddArgs{
		Stream: fmt.Sprintf("stream-%s", event.Topic),
		Values: map[string]interface{}{"event": string(bytes), "attempt": 1},
	}).Err()

}

func (r *redisStream) Consume(topic string, opts ...events.ConsumeOption) (<-chan events.Event, error) {
	if len(topic) == 0 {
		return nil, events.ErrMissingTopic
	}

	options := events.ConsumeOptions{}
	for _, o := range opts {
		o(&options)
	}
	group := options.Group
	if len(group) == 0 {
		group = uuid.New().String()
	}
	return r.consumeWithGroup(topic, group, options)
}

func (r *redisStream) consumeWithGroup(topic, group string, options events.ConsumeOptions) (<-chan events.Event, error) {
	topic = fmt.Sprintf("stream-%s", topic)
	lastRead := "$"
	if !options.Offset.IsZero() {
		lastRead = fmt.Sprintf("%d", options.Offset.Unix()*1000)
	}
	if err := r.redisClient.XGroupCreateMkStream(context.Background(), topic, group, lastRead).Err(); err != nil {
		if !strings.HasPrefix(err.Error(), "BUSYGROUP") {
			return nil, err
		}
	}
	consumerName := uuid.New().String()
	ch := make(chan events.Event)
	go func() {
		defer func() {
			// try to clean up the consumer
			r.redisClient.XGroupDelConsumer(context.Background(), topic, group, consumerName)
			close(ch)

		}()

		start := "-"
		for {

			// sweep up any old pending messages
			pend, err := r.redisClient.XPendingExt(context.Background(), &redis.XPendingExtArgs{
				Stream: topic,
				Group:  group,
				Start:  start,
				End:    "+",
				Count:  50,
			}).Result()
			if err != nil && err != redis.Nil {
				logger.Errorf("Error finding pending messages %s", err)
				return
			}
			if len(pend) == 0 {
				break
			}
			pendingIDs := make([]string, len(pend))
			for i, p := range pend {
				pendingIDs[i] = p.ID
			}

			msgs, err := r.redisClient.XClaim(context.Background(), &redis.XClaimArgs{
				Stream:   topic,
				Group:    group,
				Consumer: consumerName,
				MinIdle:  60 * time.Second,
				Messages: pendingIDs,
			}).Result()
			if err != nil {
				logger.Errorf("Error claiming message %s", err)
				return
			}
			if err := r.processMessages(msgs, ch, topic, group, options.AutoAck, options.RetryLimit); err != nil {
				logger.Errorf("Error reprocessing message %s", err)
				return
			}

			if len(pendingIDs) < 50 {
				break
			}
			start = incrementID(pendingIDs[49])
		}
		for {
			res := r.redisClient.XReadGroup(context.Background(), &redis.XReadGroupArgs{
				Group:    group,
				Consumer: consumerName,
				Streams:  []string{topic, ">"},
				Block:    0,
			})
			sl, err := res.Result()
			if err != nil && err != redis.Nil {
				logger.Errorf("Error reading from stream %s", err)
				return
			}
			if sl == nil || len(sl) == 0 || len(sl[0].Messages) == 0 {
				logger.Errorf("No data received from stream")
				return
			}

			if err := r.processMessages(sl[0].Messages, ch, topic, group, options.AutoAck, options.RetryLimit); err != nil {
				logger.Errorf("Error processing message %s", err)
				return
			}
		}
	}()
	return ch, nil
}

func (r *redisStream) processMessages(msgs []redis.XMessage, ch chan events.Event, topic, group string, autoAck bool, retryLimit int) error {
	for _, v := range msgs {
		vid := v.ID
		evBytes := v.Values["event"]
		var ev events.Event
		bStr, ok := evBytes.(string)
		if !ok {
			logger.Warnf("Failed to convert to bytes, discarding %s", vid)
			r.redisClient.XAck(context.Background(), topic, group, vid)
			continue
		}
		if err := json.Unmarshal([]byte(bStr), &ev); err != nil {
			logger.Warnf("Failed to unmarshal event, discarding %s %s", err, vid)
			r.redisClient.XAck(context.Background(), topic, group, vid)
			continue
		}
		attemptsKey := fmt.Sprintf("%s:%s:%s", topic, group, vid)
		r.Lock()
		r.attempts[attemptsKey], _ = strconv.Atoi(v.Values["attempt"].(string))
		r.Unlock()

		if !autoAck {
			ev.SetAckFunc(func() error {
				r.Lock()
				delete(r.attempts, attemptsKey)
				r.Unlock()
				err := r.redisClient.XAck(context.Background(), topic, group, vid).Err()
				return err
			})
			ev.SetNackFunc(func() error {
				// no way to nack a message. Best you can do is to ack and readd
				if err := r.redisClient.XAck(context.Background(), topic, group, vid).Err(); err != nil {
					return err
				}
				r.RLock()
				attempt := r.attempts[attemptsKey]
				r.RUnlock()
				if retryLimit > 0 && attempt > retryLimit {
					// don't readd
					r.Lock()
					delete(r.attempts, attemptsKey)
					r.Unlock()
					return nil
				}
				bytes, err := json.Marshal(ev)
				if err != nil {
					return errors.Wrap(err, "Error encoding event")
				}
				return r.redisClient.XAdd(context.Background(), &redis.XAddArgs{
					Stream: fmt.Sprintf("stream-%s", ev.Topic),
					Values: map[string]interface{}{"event": string(bytes), "attempt": attempt + 1},
				}).Err()
			})
		}
		ch <- ev
		if !autoAck {
			continue
		}
		// TODO check for error
		r.redisClient.XAck(context.Background(), topic, group, vid)
	}
	return nil
}

func incrementID(id string) string {
	// id is of form 12345-0
	parts := strings.Split(id, "-")
	if len(parts) != 2 {
		// not sure what to do with this
		return id
	}
	i, err := strconv.Atoi(parts[1])
	if err != nil {
		// not sure what to do with this
		return id
	}
	i++
	return fmt.Sprintf("%s-%d", parts[0], i)

}
