package sentinel

import (
	"context"
	"crypto/tls"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// SwitchMasterChannel is the pub/sub channel every sentinel publishes a
// completed promotion on, as "<master-name> <old-ip> <old-port> <new-ip>
// <new-port>". Each sentinel announces it on its own connection when it
// adopts the new configuration, whether it led the failover or learned the
// result from a peer.
const SwitchMasterChannel = "+switch-master"

// SubscribeSwitchMaster dials one sentinel, subscribes to +switch-master and
// hands the master name of every publication to onEvent, until the connection
// fails or ctx ends. The rest of the payload is deliberately not exposed:
// anything that can reach the sentinel port can publish on this channel, so
// the addresses in it must never steer a caller; the name is only good for
// deciding which cluster to go re-query.
//
// Errors are returned rather than retried so the caller owns the backoff; a
// nil return only follows ctx ending.
func SubscribeSwitchMaster(ctx context.Context, addr, password string, tlsCfg *tls.Config, onEvent func(masterName string)) error {
	client := redis.NewClient(&redis.Options{
		Addr:        addr,
		Password:    password,
		TLSConfig:   tlsCfg,
		DialTimeout: 5 * time.Second,
		// A healthy subscription is silent between failovers, so the read
		// must be allowed to block; Receive is given no timeout below.
	})
	defer func() { _ = client.Close() }()

	pubsub := client.Subscribe(ctx, SwitchMasterChannel)
	defer func() { _ = pubsub.Close() }()

	// A pub/sub read blocks on the socket and does not watch ctx; closing the
	// PubSub from the side is what unblocks it when the caller shuts down.
	stop := context.AfterFunc(ctx, func() { _ = pubsub.Close() })
	defer stop()

	for {
		reply, err := pubsub.Receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		msg, ok := reply.(*redis.Message)
		if !ok {
			// Subscription confirmations and pongs.
			continue
		}
		if fields := strings.Fields(msg.Payload); len(fields) > 0 {
			onEvent(fields[0])
		}
	}
}
