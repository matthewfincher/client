package chat

import (
	"context"

	"github.com/keybase/client/go/chat/storage"
	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/chat1"
	"github.com/keybase/client/go/protocol/gregor1"
	"github.com/keybase/client/go/protocol/keybase1"
)

type Syncer struct {
	libkb.Contextified
	debugLabeller

	ri func() chat1.RemoteInterface
}

func NewSyncer(g *libkb.GlobalContext, ri func() chat1.RemoteInterface) *Syncer {
	return &Syncer{
		Contextified:  libkb.NewContextified(g),
		debugLabeller: newDebugLabeller(g, "syncer"),
		ri:            ri,
	}
}

func (s *Syncer) sendChatStaleNotifications(uid gregor1.UID) {
	// Alert Electron that all chat information could be out of date. Empty conversation ID
	// list means everything needs to be refreshed
	kuid := keybase1.UID(uid.String())
	s.G().NotifyRouter.HandleChatInboxStale(context.Background(), kuid)
	s.G().NotifyRouter.HandleChatThreadsStale(context.Background(), kuid, []chat1.ConversationID{})
}

func (s *Syncer) Connected(ctx context.Context, uid gregor1.UID) error {

	// Grab the latest inbox version, and compare it to what we have
	// If we don't have the latest, then we clear the Inbox cache and
	// send alerts to clients that they should refresh.
	vers, err := s.ri().GetInboxVersion(context.Background(), uid)
	if err != nil {
		s.debug(ctx, "failed to sync inbox version: uid: %s error: %s", uid, err.Error())
		return err
	}

	ibox := storage.NewInbox(s.G(), uid, func() libkb.SecretUI {
		return DelivererSecretUI{}
	})
	// If we miss here, then let's send notifications out to clients letting
	// them know everything is hosed
	if verr := ibox.VersionSync(vers); verr != nil {
		s.debug(ctx, "error during version sync: %s", verr.Error())
		s.sendChatStaleNotifications(uid)
	}

	// Let the Deliverer know that we are back online
	s.G().MessageDeliverer.Connected()

	return nil
}

func (s *Syncer) Disconnected(ctx context.Context) {

	// Let the Deliverer know we are offline
	s.G().MessageDeliverer.Disconnected()
}
