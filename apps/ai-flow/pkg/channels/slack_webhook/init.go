package slackwebhook

import (
	"github.com/computeflux/ai-flow/pkg/bus"
	"github.com/computeflux/ai-flow/pkg/channels"
	"github.com/computeflux/ai-flow/pkg/config"
)

func init() {
	channels.RegisterFactory(
		config.ChannelSlackWebHook,
		func(channelName, channelType string, cfg *config.Config, b *bus.MessageBus) (channels.Channel, error) {
			bc := cfg.Channels[channelName]
			decoded, err := bc.GetDecoded()
			if err != nil {
				return nil, err
			}
			c, ok := decoded.(*config.SlackWebhookSettings)
			if !ok {
				return nil, channels.ErrSendFailed
			}
			ch, err := NewSlackWebhookChannel(bc, c, b)
			if err != nil {
				return nil, err
			}
			if channelName != config.ChannelSlackWebHook {
				ch.SetName(channelName)
			}
			return ch, nil
		},
	)
}
