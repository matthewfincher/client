package storage

import (
	"fmt"
	"sync"

	"time"

	"bytes"

	"sort"

	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/chat1"
	"github.com/keybase/client/go/protocol/gregor1"
)

const inboxVersion = 2

type inboxDiskQuery struct {
	Query      *chat1.GetInboxLocalQuery `codec:"Q"`
	Pagination *chat1.Pagination         `codec:"P"`
}

func (q inboxDiskQuery) queryMatch(other inboxDiskQuery) bool {
	if q.Query == nil && other.Query == nil {
		return true
	} else if q.Query != nil && other.Query != nil {
		return q.Query.Eq(*other.Query)
	}
	return false
}

func (q inboxDiskQuery) paginationMatch(other inboxDiskQuery) bool {
	if q.Pagination == nil && other.Pagination == nil {
		return true
	} else if q.Pagination != nil && other.Pagination != nil {
		return q.Pagination.Eq(*other.Pagination)
	}
	return false
}

func (q inboxDiskQuery) match(other inboxDiskQuery) bool {
	return q.queryMatch(other) && q.paginationMatch(other)
}

type inboxDiskData struct {
	Version       int                       `codec:"V"`
	InboxVersion  chat1.InboxVers           `codec:"I"`
	Conversations []chat1.ConversationLocal `codec:"C"`
	Queries       []inboxDiskQuery          `codec:"Q"`
}

type Inbox struct {
	sync.Mutex
	libkb.Contextified
	*baseBox

	uid gregor1.UID
}

func NewInbox(g *libkb.GlobalContext, uid gregor1.UID, getSecretUI func() libkb.SecretUI) *Inbox {
	return &Inbox{
		Contextified: libkb.NewContextified(g),
		baseBox:      newBaseBox(g, getSecretUI),
		uid:          uid,
	}
}

func (i *Inbox) debug(msg string, args ...interface{}) {
	i.G().Log.Debug("Inbox(uid="+i.uid.String()+": "+msg, args...)
}

func (i *Inbox) dbKey() libkb.DbKey {
	return libkb.DbKey{
		Typ: libkb.DBChatInbox,
		Key: fmt.Sprintf("ib:%s", i.uid),
	}
}

func (i *Inbox) dbKeyQueries() libkb.DbKey {
	return libkb.DbKey{
		Typ: libkb.DBChatInbox,
		Key: fmt.Sprintf("ibq:%s", i.uid),
	}
}

func (i *Inbox) readDiskInbox() (inboxDiskData, libkb.ChatStorageError) {
	var ibox inboxDiskData
	found, err := i.readDiskBox(i.dbKey(), &ibox)
	if err != nil {
		return ibox, libkb.NewChatStorageInternalError(i.G(),
			"failed to read inbox: uid: %d err: %s", i.uid, err.Error())
	}
	if !found {
		return ibox, libkb.ChatStorageMissError{}
	}
	if ibox.Version > inboxVersion {
		return ibox, libkb.NewChatStorageInternalError(i.G(),
			"invalid inbox version: %d (current: %d)", ibox.Version, inboxVersion)
	}
	return ibox, nil
}

func (i *Inbox) writeDiskInbox(ibox inboxDiskData) libkb.ChatStorageError {
	ibox.Version = inboxVersion
	if ierr := i.writeDiskBox(i.dbKey(), ibox); ierr != nil {
		return libkb.NewChatStorageInternalError(i.G(), "failed to write inbox: uid: %s err: %s",
			i.uid, ierr.Error())
	}
	return nil
}

type ByDatabaseOrder []chat1.ConversationLocal

func (a ByDatabaseOrder) Len() int      { return len(a) }
func (a ByDatabaseOrder) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a ByDatabaseOrder) Less(i, j int) bool {
	if a[i].ReaderInfo.Mtime < a[j].ReaderInfo.Mtime {
		return true
	} else if a[i].ReaderInfo.Mtime > a[j].ReaderInfo.Mtime {
		return false
	}
	return bytes.Compare(a[i].Info.Id, a[j].Info.Id) < 0
}

func (i *Inbox) mergeConvs(l []chat1.ConversationLocal, r []chat1.ConversationLocal) (res []chat1.ConversationLocal) {
	m := make(map[string]bool)
	for _, conv := range l {
		m[conv.Info.Id.String()] = true
		res = append(res, conv)
	}
	for _, conv := range r {
		if !m[conv.Info.Id.String()] {
			res = append(res, conv)
		}
	}
	sort.Sort(ByDatabaseOrder(res))
	return res
}

func (i *Inbox) Merge(vers chat1.InboxVers, convs []chat1.ConversationLocal,
	query *chat1.GetInboxLocalQuery, p *chat1.Pagination) libkb.ChatStorageError {
	i.Lock()
	defer i.Unlock()

	i.debug("Merge: vers: %d", vers)

	// Read inbox off disk to determine if we can merge, or need to full replace
	ibox, err := i.readDiskInbox()
	if err != nil {
		if _, ok := err.(libkb.ChatStorageMissError); !ok {
			return err
		}
	}

	// Replace the inbox under these conditions
	qp := inboxDiskQuery{Query: query, Pagination: p}
	var data inboxDiskData
	if ibox.InboxVersion != vers || err != nil {
		i.debug("Merge: replacing inbox: ibox.vers: %v vers: %v", ibox.InboxVersion, vers)
		data = inboxDiskData{
			Version:       inboxVersion,
			InboxVersion:  vers,
			Conversations: convs,
			Queries:       []inboxDiskQuery{qp},
		}
	} else {
		i.debug("Merge: merging inbox: version match")
		data = inboxDiskData{
			Version:       inboxVersion,
			InboxVersion:  vers,
			Conversations: i.mergeConvs(convs, ibox.Conversations),
			Queries:       append(ibox.Queries, qp),
		}
	}

	// Write out new inbox
	if err := i.writeDiskInbox(data); err != nil {
		return err
	}
	return nil
}

func (i *Inbox) applyQuery(query *chat1.GetInboxLocalQuery, convs []chat1.ConversationLocal) []chat1.ConversationLocal {
	if query == nil {
		return convs
	}
	var res []chat1.ConversationLocal
	for _, conv := range convs {
		ok := true
		if query.ConvID != nil && !query.ConvID.Eq(conv.Info.Id) {
			ok = false
		} else if query.After != nil && !query.After.After(conv.ReaderInfo.Mtime) {
			ok = false
		} else if query.Before != nil && !query.Before.Before(conv.ReaderInfo.Mtime) {
			ok = false
		} else if query.TopicName != nil && *query.TopicName != conv.Info.TopicName {
			ok = false
		} else if query.TopicType != nil && *query.TopicType != conv.Info.Triple.TopicType {
			ok = false
		} else if query.TlfVisibility != nil && *query.TlfVisibility != conv.Info.Visibility {
			ok = false
		} else if query.UnreadOnly && conv.ReaderInfo.ReadMsgid >= conv.ReaderInfo.MaxMsgid {
			ok = false
		} else if query.TlfName != nil && *query.TlfName != conv.Info.TlfName {
			ok = false
		} else if query.ReadOnly && conv.ReaderInfo.ReadMsgid < conv.ReaderInfo.MaxMsgid {
			ok = false
		}
		if ok {
			res = append(res, conv)
		}
		// TODO: Status filter
		// TODO: ComputeActiveList
		// TODO: OneChatTypePerTLF
	}
	return res
}

func (i *Inbox) queryExists(ibox inboxDiskData, query *chat1.GetInboxLocalQuery,
	p *chat1.Pagination) bool {
	for _, q := range ibox.Queries {
		if q.match(inboxDiskQuery{Query: query, Pagination: p}) {
			return true
		}
	}
	return false
}

func (i *Inbox) Read(query *chat1.GetInboxLocalQuery, p *chat1.Pagination) (chat1.InboxVers, []chat1.ConversationLocal, libkb.ChatStorageError) {
	i.Lock()
	defer i.Unlock()

	ibox, err := i.readDiskInbox()
	if err != nil {
		return 0, nil, err
	}

	// Check to make sure query parameters have been seen before
	if !i.queryExists(ibox, query, p) {
		i.debug("Read: miss: query or pagination unknown")
		return 0, nil, libkb.ChatStorageMissError{}
	}
	ibox.Conversations = i.applyQuery(query, ibox.Conversations)
	// TODO pagination

	i.debug("Read: hit: version: %d", ibox.InboxVersion)
	return ibox.InboxVersion, ibox.Conversations, nil
}

func (i *Inbox) clear() libkb.ChatStorageError {
	err := i.G().LocalChatDb.Delete(i.dbKey())
	if err != nil {
		return libkb.NewChatStorageInternalError(i.G(), "error clearing inbox: uid: %s err: %s", i.uid,
			err.Error())
	}
	return nil
}

func (i *Inbox) handleVersion(ourvers chat1.InboxVers, updatevers chat1.InboxVers) (chat1.InboxVers, bool, libkb.ChatStorageError) {
	// Our version is at least as new as this update, let's not continue
	if updatevers == 0 {
		i.debug("handleVersion: received an self update: ours: %d update: %d", ourvers, updatevers)
		return ourvers + 1, true, nil
	} else if ourvers >= updatevers {
		i.debug("handleVersion: received an old update: ours: %d update: %d", ourvers, updatevers)
		return ourvers, false, nil
	} else if updatevers == ourvers+1 {
		i.debug("handleVersion: received an incremental update: ours: %d update: %d", ourvers, updatevers)
		return updatevers, true, nil
	}

	i.debug("handleVersion: received a non-incremental update, clearing: ours: %d update: %d", ourvers, updatevers)
	return ourvers, false, i.clear()
}

func (i *Inbox) NewConversation(vers chat1.InboxVers, conv chat1.ConversationLocal) error {
	i.Lock()
	defer i.Unlock()

	i.debug("NewConversation: vers: %d convID: %s", vers, conv.Info.Id)
	ibox, err := i.readDiskInbox()
	if err != nil {
		return err
	}

	// Check inbox versions, make sure it makes sense (clear otherwise)
	var cont bool
	if vers, cont, err = i.handleVersion(ibox.InboxVersion, vers); !cont {
		return err
	}

	// Find any conversations this guy might supersede and set supersededBy pointer
	for index := range ibox.Conversations {
		iconv := &ibox.Conversations[index]
		if iconv.Info.FinalizeInfo != nil {
			continue
		}
		for _, super := range conv.Supersedes {
			if iconv.Info.Id.Eq(super) {
				iconv.SupersededBy = append(iconv.SupersededBy, conv.Info.Id)
			}
		}
	}

	// Add the convo
	ibox.Conversations = append([]chat1.ConversationLocal{conv}, ibox.Conversations...)

	// Write out to disk
	ibox.InboxVersion = vers
	if err := i.writeDiskInbox(ibox); err != nil {
		return err
	}

	return nil
}

func (i *Inbox) getConv(convID chat1.ConversationID, convs []chat1.ConversationLocal) (int, *chat1.ConversationLocal) {

	var index int
	var conv chat1.ConversationLocal
	found := false
	for index, conv = range convs {
		if conv.Info.Id.Eq(convID) {
			found = true
			break
		}
	}
	if !found {
		return 0, nil
	}

	return index, &convs[index]
}

func (i *Inbox) NewMessage(vers chat1.InboxVers, convID chat1.ConversationID, msg chat1.MessageUnboxed) libkb.ChatStorageError {
	i.Lock()
	defer i.Unlock()

	i.debug("NewMessage: vers: %d convID: %s", vers, convID)
	ibox, err := i.readDiskInbox()
	if err != nil {
		return err
	}

	// Check inbox versions, make sure it makes sense (clear otherwise)
	var cont bool
	if vers, cont, err = i.handleVersion(ibox.InboxVersion, vers); !cont {
		return err
	}

	// Find conversation
	index, conv := i.getConv(convID, ibox.Conversations)
	if conv == nil {
		i.debug("NewMessage: no conversation found: convID: %s, clearing", convID)
		return i.clear()
	}

	// Update conversation
	found := false
	typ := msg.GetMessageType()
	for mindex, maxmsg := range conv.MaxMessages {
		if maxmsg.GetMessageType() == typ {
			conv.MaxMessages[mindex] = msg
			found = true
			break
		}
	}
	if !found {
		conv.MaxMessages = append(conv.MaxMessages, msg)
	}
	conv.ReaderInfo.MaxMsgid = msg.GetMessageID()
	conv.ReaderInfo.Mtime = gregor1.ToTime(time.Now())
	// TODO: How do we handle ReaderNames?

	// Slot in at the top
	i.debug("NewMessage: promoting convID: %s to the top of %d convs", convID, len(ibox.Conversations))
	ibox.Conversations = append(ibox.Conversations[:index], ibox.Conversations[index+1:]...)
	ibox.Conversations = append([]chat1.ConversationLocal{*conv}, ibox.Conversations...)

	// Write out to disk
	ibox.InboxVersion = vers
	if err := i.writeDiskInbox(ibox); err != nil {
		return err
	}

	return nil
}

func (i *Inbox) ReadMessage(vers chat1.InboxVers, convID chat1.ConversationID, msgID chat1.MessageID) libkb.ChatStorageError {
	i.Lock()
	defer i.Unlock()

	i.debug("ReadMessage: vers: %d convID: %s", vers, convID)
	ibox, err := i.readDiskInbox()
	if err != nil {
		return err
	}

	// Check inbox versions, make sure it makes sense (clear otherwise)
	var cont bool
	if vers, cont, err = i.handleVersion(ibox.InboxVersion, vers); !cont {
		return err
	}

	// Find conversation
	_, conv := i.getConv(convID, ibox.Conversations)
	if conv == nil {
		i.debug("ReadMessage: no conversation found: convID: %s, clearing", convID)
		return i.clear()
	}

	// Update conv
	conv.ReaderInfo.Mtime = gregor1.ToTime(time.Now())
	conv.ReaderInfo.ReadMsgid = msgID

	// Write out to disk
	ibox.InboxVersion = vers
	if err := i.writeDiskInbox(ibox); err != nil {
		return err
	}

	return nil
}

func (i *Inbox) SetStatus(vers chat1.InboxVers, convID chat1.ConversationID, status chat1.ConversationStatus) libkb.ChatStorageError {
	i.Lock()
	defer i.Unlock()

	i.debug("SetStatus: vers: %d convID: %s", vers, convID)
	ibox, err := i.readDiskInbox()
	if err != nil {
		return err
	}

	// Check inbox versions, make sure it makes sense (clear otherwise)
	var cont bool
	if vers, cont, err = i.handleVersion(ibox.InboxVersion, vers); !cont {
		return err
	}

	// Find conversation
	index, conv := i.getConv(convID, ibox.Conversations)
	if conv == nil {
		i.debug("SetStatus: no conversation found: convID: %s, clearing", convID)
		return i.clear()
	}

	// Update conv
	if status == chat1.ConversationStatus_IGNORED || status == chat1.ConversationStatus_BLOCKED {
		// Remove conv
		ibox.Conversations = append(ibox.Conversations[:index], ibox.Conversations[index+1:]...)
	}
	conv.ReaderInfo.Mtime = gregor1.ToTime(time.Now())

	// Write out to disk
	ibox.InboxVersion = vers
	if err := i.writeDiskInbox(ibox); err != nil {
		return err
	}

	return nil
}

func (i *Inbox) TlfFinalize(vers chat1.InboxVers, convIDs []chat1.ConversationID,
	finalizeInfo chat1.ConversationFinalizeInfo) libkb.ChatStorageError {
	i.Lock()
	defer i.Unlock()

	i.debug("TlfFinalize: vers: %d convIDs: %v finalizeInfo: %v", vers, convIDs, finalizeInfo)
	ibox, err := i.readDiskInbox()
	if err != nil {
		return err
	}

	// Check inbox versions, make sure it makes sense (clear otherwise)
	var cont bool
	if vers, cont, err = i.handleVersion(ibox.InboxVersion, vers); !cont {
		return err
	}

	for _, convID := range convIDs {
		// Find conversation
		_, conv := i.getConv(convID, ibox.Conversations)
		if conv == nil {
			i.debug("TlfFinalize: no conversation found: convID: %s", convID)
			continue
		}

		conv.Info.FinalizeInfo = &finalizeInfo
	}

	// Write out to disk
	ibox.InboxVersion = vers
	if err := i.writeDiskInbox(ibox); err != nil {
		return err
	}

	return nil
}
