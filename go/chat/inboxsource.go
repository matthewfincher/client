package chat

import (
	"errors"
	"fmt"

	"strings"

	"github.com/keybase/client/go/chat/storage"
	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/chat1"
	"github.com/keybase/client/go/protocol/gregor1"
	"github.com/keybase/client/go/protocol/keybase1"
	"golang.org/x/net/context"
	"golang.org/x/sync/errgroup"
)

type Inbox struct {
	Version         chat1.InboxVers
	ConvsUnverified []chat1.Conversation
	Convs           []chat1.ConversationLocal
	Pagination      *chat1.Pagination
}

type InboxSource interface {
	// Read reads inbox from the source. We specify the identify behavior as a
	// parameter here to give it a chance to re-run identify an get latest
	// identify results even when pulling from local storage. The local storage
	// doesn't include identify information such as proof breaks.
	Read(ctx context.Context, uid gregor1.UID, query *chat1.GetInboxLocalQuery,
		p *chat1.Pagination) (Inbox, *chat1.RateLimit, error)
}

type localizer struct {
	libkb.Contextified
	getTlfInterface func() keybase1.TlfInterface
}

func newLocalizer(g *libkb.GlobalContext, getTlfInterface func() keybase1.TlfInterface) *localizer {
	return &localizer{
		Contextified:    libkb.NewContextified(g),
		getTlfInterface: getTlfInterface,
	}
}

type RemoteInboxSource struct {
	libkb.Contextified

	localizer        *localizer
	boxer            *Boxer
	getTlfInterface  func() keybase1.TlfInterface
	getChatInterface func() chat1.RemoteInterface
}

func NewRemoteInboxSource(g *libkb.GlobalContext, boxer *Boxer, ri func() chat1.RemoteInterface,
	tlf func() keybase1.TlfInterface) *RemoteInboxSource {
	return &RemoteInboxSource{
		Contextified:     libkb.NewContextified(g),
		localizer:        newLocalizer(g, tlf),
		getTlfInterface:  tlf,
		getChatInterface: ri,
		boxer:            boxer,
	}
}

func (s *RemoteInboxSource) Read(ctx context.Context, uid gregor1.UID,
	query *chat1.GetInboxLocalQuery, p *chat1.Pagination) (
	Inbox, *chat1.RateLimit, error) {

	rquery, tlfInfo, err := GetInboxQueryLocalToRemote(ctx, s.getTlfInterface(), query)
	if err != nil {
		return Inbox{}, nil, err
	}
	ib, err := s.getChatInterface().GetInboxRemote(ctx, chat1.GetInboxRemoteArg{
		Query:      rquery,
		Pagination: p,
	})
	if err != nil {
		return Inbox{}, ib.RateLimit, err
	}

	var res []chat1.ConversationLocal
	convLocals, err :=
		s.localizer.localizeConversationsPipeline(ctx, uid, ib.Inbox.Full().Conversations, nil)
	if err != nil {
		return Inbox{}, ib.RateLimit, err
	}
	for _, convLocal := range convLocals {
		if rquery != nil && rquery.TlfID != nil {
			// inbox query contained a TLF name, so check to make sure that
			// the conversation from the server matches tlfInfo from kbfs
			if convLocal.Info.TlfName != tlfInfo.CanonicalName {
				return Inbox{}, ib.RateLimit, fmt.Errorf("server conversation TLF name mismatch: %s, expected %s", convLocal.Info.TlfName, tlfInfo.CanonicalName)
			}
			if convLocal.Info.Visibility != rquery.Visibility() {
				return Inbox{}, ib.RateLimit, fmt.Errorf("server conversation TLF visibility mismatch: %s, expected %s", convLocal.Info.Visibility, rquery.Visibility())
			}
			if !tlfInfo.ID.Eq(convLocal.Info.Triple.Tlfid) {
				return Inbox{}, ib.RateLimit, fmt.Errorf("server conversation TLF ID mismatch: %s, expected %s", convLocal.Info.Triple.Tlfid, tlfInfo.ID)
			}
			// tlfInfo.ID and rquery.TlfID should always match, but just in case:
			if !rquery.TlfID.Eq(convLocal.Info.Triple.Tlfid) {
				return Inbox{}, ib.RateLimit, fmt.Errorf("server conversation TLF ID mismatch: %s, expected %s", convLocal.Info.Triple.Tlfid, rquery.TlfID)
			}

			// Note that previously, we made a call to KBFS to lookup the TLF in
			// convLocal.Info.TlfName and verify that, but the above checks accomplish
			// the same thing without an RPC call.
		}

		// server can't query on topic name, so we have to do it ourselves in the loop
		if query != nil && query.TopicName != nil && *query.TopicName != convLocal.Info.TopicName {
			continue
		}

		res = append(res, convLocal)
	}

	return Inbox{
		Version:    ib.Inbox.Full().Vers,
		Convs:      res,
		Pagination: ib.Inbox.Full().Pagination,
	}, ib.RateLimit, nil
}

type NonblockInboxResult struct {
	ConvID   chat1.ConversationID
	Err      error
	ConvRes  *chat1.ConversationLocal
	InboxRes *Inbox
}

type NonblockRemoteInboxSource struct {
	libkb.Contextified

	localizer        *localizer
	boxer            *Boxer
	localizeCb       chan NonblockInboxResult
	getTlfInterface  func() keybase1.TlfInterface
	getChatInterface func() chat1.RemoteInterface
}

func NewNonblockRemoteInboxSource(g *libkb.GlobalContext, boxer *Boxer, ri func() chat1.RemoteInterface,
	tlf func() keybase1.TlfInterface, localizeCb chan NonblockInboxResult) *NonblockRemoteInboxSource {
	return &NonblockRemoteInboxSource{
		Contextified:     libkb.NewContextified(g),
		getTlfInterface:  tlf,
		getChatInterface: ri,
		boxer:            boxer,
		localizeCb:       localizeCb,
		localizer:        newLocalizer(g, tlf),
	}
}

func (s *NonblockRemoteInboxSource) Read(ctx context.Context, uid gregor1.UID,
	query *chat1.GetInboxLocalQuery, p *chat1.Pagination) (
	Inbox, *chat1.RateLimit, error) {

	rquery, _, err := GetInboxQueryLocalToRemote(ctx, s.getTlfInterface(), query)
	if err != nil {
		return Inbox{}, nil, err
	}
	ib, err := s.getChatInterface().GetInboxRemote(ctx, chat1.GetInboxRemoteArg{
		Query:      rquery,
		Pagination: p,
	})
	if err != nil {
		return Inbox{}, ib.RateLimit, err
	}
	inbox := Inbox{
		Version:         ib.Inbox.Full().Vers,
		ConvsUnverified: ib.Inbox.Full().Conversations,
		Pagination:      ib.Inbox.Full().Pagination,
	}

	// Send inbox over localize channel
	s.localizeCb <- NonblockInboxResult{
		InboxRes: &inbox,
	}

	// Spawn off localization into its own goroutine and use cb to communicate with outside world
	bctx := BackgroundContext(ctx)
	go func() {
		s.localizer.localizeConversationsPipeline(bctx, uid, ib.Inbox.Full().Conversations,
			&s.localizeCb)

		// Shutdown localize channel
		close(s.localizeCb)
	}()

	return inbox, ib.RateLimit, nil
}

type HybridInboxSource struct {
	libkb.Contextified

	remote    *RemoteInboxSource
	inbox     *storage.Inbox
	localizer *localizer
}

func NewHybridInboxSource(g *libkb.GlobalContext, inbox *storage.Inbox, remote *RemoteInboxSource) *HybridInboxSource {
	return &HybridInboxSource{
		Contextified: libkb.NewContextified(g),
		remote:       remote,
		inbox:        inbox,
		localizer:    newLocalizer(g, remote.getTlfInterface),
	}
}

func (s *HybridInboxSource) finalizeConvs(ctx context.Context, uid gregor1.UID,
	convs []chat1.ConversationLocal) (res []chat1.ConversationLocal, err error) {

	// Gather up non-permanent errors
	var convErrs []chat1.Conversation
	for _, conv := range convs {
		if conv.Error != nil && !conv.Error.Permanent {
			convErrs = append(convErrs, conv.Error.RemoteConv)
		}
	}

	// Localize them
	s.G().Log.Debug("HybridInboxSource: finalizing %d conversations", len(convErrs))
	convLocals, err := s.localizer.localizeConversationsPipeline(ctx, uid, convErrs, nil)
	if err != nil {
		return nil, err
	}

	// Form final result
	m := make(map[string]chat1.ConversationLocal)
	for _, conv := range convLocals {
		m[conv.Info.Id.String()] = conv
	}
	for _, conv := range convs {
		if conv.Error != nil && !conv.Error.Permanent {
			res = append(res, m[conv.Info.Id.String()])
		} else {
			res = append(res, conv)
		}
	}

	return res, nil
}

func (s *HybridInboxSource) Read(ctx context.Context, uid gregor1.UID, query *chat1.GetInboxLocalQuery,
	p *chat1.Pagination) (Inbox, *chat1.RateLimit, error) {

	// Try local storage
	vers, convs, pagination, cerr := s.inbox.Read(query, p)
	if cerr != nil {
		if _, ok := cerr.(libkb.ChatStorageMissError); !ok {
			s.G().Log.Error("HybridInboxSource: error fetch inbox locally: %s", cerr.Error())
		}
	} else {
		s.G().Log.Debug("HybridInboxSource: hit local storage: uid: %s convs: %d", uid, len(convs))
		finalConvs, err := s.finalizeConvs(ctx, uid, convs)
		if err != nil {
			return Inbox{}, nil, err
		}
		return Inbox{
			Version:    vers,
			Convs:      finalConvs,
			Pagination: pagination,
		}, nil, nil
	}

	// Go to the remote on miss
	ib, rl, err := s.remote.Read(ctx, uid, query, p)
	if err != nil {
		return Inbox{}, rl, err
	}

	// Write out to local storage
	if cerr := s.inbox.Merge(ib.Version, ib.Convs, query, p); cerr != nil {
		return Inbox{}, rl, cerr
	}

	return ib, rl, nil
}

func (s *localizer) localizeConversationsPipeline(ctx context.Context, uid gregor1.UID,
	convs []chat1.Conversation, localizeCb *chan NonblockInboxResult) ([]chat1.ConversationLocal, error) {

	// Fetch conversation local information in parallel
	type jobRes struct {
		conv  chat1.ConversationLocal
		index int
	}
	type job struct {
		conv  chat1.Conversation
		index int
	}
	eg, ctx := errgroup.WithContext(ctx)
	convCh := make(chan job)
	retCh := make(chan jobRes)
	eg.Go(func() error {
		defer close(convCh)
		for i, conv := range convs {
			select {
			case convCh <- job{conv: conv, index: i}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	for i := 0; i < 10; i++ {
		eg.Go(func() error {
			for conv := range convCh {
				convLocal := s.localizeConversation(ctx, uid, conv.conv)

				jr := jobRes{
					conv:  convLocal,
					index: conv.index,
				}
				select {
				case retCh <- jr:
				case <-ctx.Done():
					return ctx.Err()
				}

				// If a localize callback channel exists, send along the result as well
				if localizeCb != nil {
					if convLocal.Error != nil {
						*localizeCb <- NonblockInboxResult{
							Err:    errors.New(convLocal.Error.Message),
							ConvID: conv.conv.Metadata.ConversationID,
						}
					} else {
						*localizeCb <- NonblockInboxResult{
							ConvRes: &convLocal,
							ConvID:  convLocal.Info.Id,
						}
					}
				}
			}
			return nil
		})
	}
	go func() {
		eg.Wait()
		close(retCh)
	}()
	res := make([]chat1.ConversationLocal, len(convs))
	for c := range retCh {
		res[c.index] = c.conv
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return res, nil
}

func (s *localizer) needsCanonicalize(name string) bool {
	return strings.Contains(name, "@") || strings.Contains(name, ":")
}

func (s *localizer) isErrPermanent(err error) bool {
	if uberr, ok := err.(libkb.ChatUnboxingError); ok {
		return uberr.IsPermanent()
	}
	return false
}

func (s *localizer) localizeConversation(ctx context.Context, uid gregor1.UID,
	conversationRemote chat1.Conversation) (conversationLocal chat1.ConversationLocal) {

	s.G().Log.Debug("localizeConversation: localizing %d msgs", len(conversationRemote.MaxMsgs))

	conversationLocal.Info = chat1.ConversationInfoLocal{
		Id:         conversationRemote.Metadata.ConversationID,
		Visibility: conversationRemote.Metadata.Visibility,
		Triple:     conversationRemote.Metadata.IdTriple,
		Status:     conversationRemote.Metadata.Status,
	}
	conversationLocal.Info.FinalizeInfo = conversationRemote.Metadata.FinalizeInfo
	for _, super := range conversationRemote.Supersedes {
		conversationLocal.Supersedes = append(conversationLocal.Supersedes, super.ConversationID)
	}
	for _, super := range conversationRemote.SupersededBy {
		conversationLocal.SupersededBy = append(conversationLocal.SupersededBy, super.ConversationID)
	}
	if conversationRemote.ReaderInfo == nil {
		errMsg := "empty ReaderInfo from server?"
		conversationLocal.Error = &chat1.ConversationErrorLocal{
			Message:    errMsg,
			RemoteConv: conversationRemote,
			Permanent:  false,
		}
		return conversationLocal
	}
	conversationLocal.ReaderInfo = *conversationRemote.ReaderInfo

	if len(conversationRemote.MaxMsgs) == 0 {
		errMsg := "conversation has an empty MaxMsgs field"
		conversationLocal.Error = &chat1.ConversationErrorLocal{
			Message:    errMsg,
			RemoteConv: conversationRemote,
			Permanent:  false,
		}
		return conversationLocal
	}

	var msgIDs []chat1.MessageID
	for _, m := range conversationRemote.MaxMsgs {
		msgIDs = append(msgIDs, m.GetMessageID())
	}

	var err error
	conversationLocal.MaxMessages, err = s.G().ConvSource.GetMessages(ctx,
		conversationRemote.Metadata.ConversationID, uid, msgIDs)
	if err != nil {
		conversationLocal.Error = &chat1.ConversationErrorLocal{
			Message:    err.Error(),
			RemoteConv: conversationRemote,
			Permanent:  s.isErrPermanent(err),
		}
		return conversationLocal
	}

	// Set to true later if visible messages are in max messages.
	conversationLocal.IsEmpty = true

	var maxValidID chat1.MessageID
	for _, mm := range conversationLocal.MaxMessages {
		if mm.IsValid() {
			body := mm.Valid().MessageBody
			typ, err := body.MessageType()
			if err != nil {
				s.G().Log.Debug("localizeConversation: failed to get message type: convID: %s id: %d",
					conversationRemote.Metadata.ConversationID, mm.GetMessageID())
				continue
			}
			if typ == chat1.MessageType_METADATA {
				conversationLocal.Info.TopicName = body.Metadata().ConversationTitle
			}
			if IsVisibleChatMessageType(typ) {
				conversationLocal.IsEmpty = false
			}

			if mm.GetMessageID() >= maxValidID {
				conversationLocal.Info.TlfName = mm.Valid().ClientHeader.TlfName
				maxValidID = mm.GetMessageID()
			}
			conversationLocal.Info.Triple = mm.Valid().ClientHeader.Conv
		}
	}

	if len(conversationLocal.Info.TlfName) == 0 {
		errMsg := "no valid message in the conversation"
		conversationLocal.Error = &chat1.ConversationErrorLocal{
			Message:    errMsg,
			RemoteConv: conversationRemote,
			Permanent:  false,
		}
		return conversationLocal
	}

	// Verify ConversationID is derivable from ConversationIDTriple
	if !conversationLocal.Info.Triple.Derivable(conversationLocal.Info.Id) {
		errMsg := fmt.Sprintf("unexpected response from server: conversation ID is not derivable from conversation triple. triple: %#+v; Id: %x",
			conversationLocal.Info.Triple, conversationLocal.Info.Id)
		conversationLocal.Error = &chat1.ConversationErrorLocal{
			Message:    errMsg,
			RemoteConv: conversationRemote,
			Permanent:  false,
		}
		return conversationLocal
	}

	// Only do this check if there is a chance the TLF name might be an SBS name.
	if s.needsCanonicalize(conversationLocal.Info.TlfName) {
		info, err := LookupTLF(ctx, s.getTlfInterface(), conversationLocal.Info.TlfName, conversationLocal.Info.Visibility)
		if err != nil {
			errMsg := err.Error()
			conversationLocal.Error = &chat1.ConversationErrorLocal{
				Message:    errMsg,
				RemoteConv: conversationRemote,
				Permanent:  s.isErrPermanent(err),
			}
			return conversationLocal
		}
		// Not sure about the utility of this TlfName assignment, but the previous code did this:
		conversationLocal.Info.TlfName = info.CanonicalName
	}

	conversationLocal.Info.WriterNames, conversationLocal.Info.ReaderNames, err = ReorderParticipants(
		ctx,
		s.G().GetUPAKLoader(),
		conversationLocal.Info.TlfName,
		conversationRemote.Metadata.ActiveList)
	if err != nil {
		errMsg := fmt.Sprintf("error reordering participants: %v", err.Error())
		conversationLocal.Error = &chat1.ConversationErrorLocal{
			Message:    errMsg,
			RemoteConv: conversationRemote,
			Permanent:  s.isErrPermanent(err),
		}
		return conversationLocal
	}

	// verify Conv matches ConversationIDTriple in MessageClientHeader
	if !conversationRemote.Metadata.IdTriple.Eq(conversationLocal.Info.Triple) {
		errMsg := "server header conversation triple does not match client header triple"
		conversationLocal.Error = &chat1.ConversationErrorLocal{
			Message:    errMsg,
			RemoteConv: conversationRemote,
			Permanent:  false,
		}
		return conversationLocal
	}

	return conversationLocal
}
