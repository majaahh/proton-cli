// Package imapbridge adapts the mail service to a local IMAP backend.
package imapbridge

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	stdmail "net/mail"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/memory"
	mailsvc "github.com/roman-16/proton-cli/internal/service/mail"
)

// Backend exposes Proton mailboxes through a local IMAP server.
type Backend struct {
	ctx  context.Context
	mail *mailsvc.Service

	mu          sync.Mutex
	loaded      bool
	loadErr     error
	boxes       map[string]*mailbox
	uidValidity uint32
}

// New creates an IMAP backend. It reads folder names when a client first lists
// mailboxes and loads each folder's messages when that folder is opened.
func New(ctx context.Context, mail *mailsvc.Service) (*Backend, error) {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fmt.Errorf("create IMAP mailbox identity: %w", err)
	}
	uidValidity := binary.BigEndian.Uint32(raw[:]) & 0x7fffffff
	if uidValidity == 0 {
		uidValidity = 1
	}
	return &Backend{ctx: ctx, mail: mail, uidValidity: uidValidity}, nil
}

func (b *Backend) Login(_ *imap.ConnInfo, username, _ string) (backend.User, error) {
	slog.DebugContext(b.ctx, "local IMAP client authenticated")
	// The enclosing IMAP server listens on loopback only. The local login fields
	// identify a client in Thunderbird; Proton authentication uses the CLI profile.
	return &user{username: username, backend: b}, nil
}

func (b *Backend) loadMailboxes() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.loaded {
		return b.loadErr
	}
	b.loadErr = nil
	b.boxes = make(map[string]*mailbox)

	boxes, err := b.mail.Mailboxes(b.ctx)
	if err != nil {
		b.loadErr = err
		return err
	}
	for _, box := range boxes {
		if !box.Folder {
			continue
		}
		name := box.Name
		if strings.EqualFold(name, "inbox") {
			name = "INBOX"
		}
		key := name
		if strings.EqualFold(name, "INBOX") {
			key = "INBOX"
		}
		b.boxes[key] = &mailbox{
			Mailbox: memory.Mailbox{Subscribed: true}, name: name, id: box.ID,
			backend: b, uidValidity: b.uidValidity,
		}
	}
	if _, ok := b.boxes["INBOX"]; !ok {
		b.boxes["INBOX"] = &mailbox{
			Mailbox: memory.Mailbox{Subscribed: true}, name: "INBOX", backend: b,
			uidValidity: b.uidValidity,
		}
	}
	b.loaded = true
	return nil
}

func (b *Backend) listMailbox(box mailsvc.Mailbox) ([]mailsvc.Message, int, error) {
	const pageSize = 150
	var rows []mailsvc.Message
	total := 0
	for page := 0; ; page++ {
		part, count, err := b.mail.List(b.ctx, mailsvc.ListOptions{
			Folder: box.ID, Page: page, PageSize: pageSize,
		})
		if err != nil {
			return nil, 0, err
		}
		total = count
		rows = append(rows, part...)
		if len(rows) >= total || len(part) == 0 {
			break
		}
	}
	// IMAP sequence numbers follow mailbox order, oldest first. Preserve that
	// order and assign UIDs for this server snapshot.
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, total, nil
}

func (b *Backend) exportMessage(row mailsvc.Message, uid uint32) (*memory.Message, error) {
	body, _, err := b.mail.Export(b.ctx, row.ID, true)
	if err != nil {
		return nil, err
	}
	flags := []string{}
	if row.Unread == 0 {
		flags = append(flags, imap.SeenFlag)
	}
	if row.Starred() {
		flags = append(flags, imap.FlaggedFlag)
	}
	return &memory.Message{
		Uid: uid, Date: time.Unix(row.Time, 0),
		Size: uint32(len(body)), Flags: flags, Body: body,
	}, nil
}

func metadataOnlyFetch(items []imap.FetchItem) bool {
	for _, item := range items {
		switch item {
		case imap.FetchEnvelope, imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size, imap.FetchUid:
			continue
		}
		section, err := imap.ParseBodySectionName(item)
		if err != nil || section.Specifier != imap.HeaderSpecifier || len(section.Path) != 0 {
			return false
		}
	}
	return true
}

func (b *Backend) metadataMessage(row mailsvc.Message, uid uint32) *memory.Message {
	from := (&stdmail.Address{Name: row.FromName, Address: row.FromAddress}).String()
	body := "From: " + from + "\r\n"
	if row.Subject != "" {
		body += "Subject: " + mime.QEncoding.Encode("utf-8", row.Subject) + "\r\n"
	}
	body += "Date: " + time.Unix(row.Time, 0).UTC().Format(time.RFC1123Z) + "\r\n\r\n"
	flags := []string{}
	if row.Unread == 0 {
		flags = append(flags, imap.SeenFlag)
	}
	if row.Starred() {
		flags = append(flags, imap.FlaggedFlag)
	}
	return &memory.Message{
		Uid: uid, Date: time.Unix(row.Time, 0), Size: uint32(row.Size), Flags: flags,
		Body: []byte(body),
	}
}

type user struct {
	username string
	backend  *Backend
}

func (u *user) Username() string { return u.username }

func (u *user) ListMailboxes(subscribed bool) ([]backend.Mailbox, error) {
	if err := u.backend.loadMailboxes(); err != nil {
		return nil, err
	}
	slog.DebugContext(u.backend.ctx, "local IMAP client listed mailboxes")
	u.backend.mu.Lock()
	defer u.backend.mu.Unlock()
	out := make([]backend.Mailbox, 0, len(u.backend.boxes))
	for _, box := range u.backend.boxes {
		if subscribed && !box.Subscribed {
			continue
		}
		out = append(out, box)
	}
	return out, nil
}

func (u *user) GetMailbox(name string) (backend.Mailbox, error) {
	slog.DebugContext(u.backend.ctx, "local IMAP client opened a mailbox")
	if err := u.backend.loadMailboxes(); err != nil {
		return nil, err
	}
	key := name
	if strings.EqualFold(name, "INBOX") {
		key = "INBOX"
	}
	u.backend.mu.Lock()
	box, ok := u.backend.boxes[key]
	u.backend.mu.Unlock()
	if !ok {
		return nil, backend.ErrNoSuchMailbox
	}
	return box, nil
}

func (*user) CreateMailbox(string) error { return errors.New("mailboxes are read-only") }
func (*user) DeleteMailbox(string) error { return errors.New("mailboxes are read-only") }
func (*user) RenameMailbox(string, string) error {
	return errors.New("mailboxes are read-only")
}
func (*user) Logout() error { return nil }

type mailbox struct {
	memory.Mailbox
	backend     *Backend
	id          string
	name        string
	uidValidity uint32
	loadMu      sync.Mutex
	rows        []mailsvc.Message
	rowsLoaded  bool
	loaded      bool
	count       uint32
	unseen      uint32
	countLoaded bool
}

func (m *mailbox) loadMessages() error {
	if err := m.loadRows(); err != nil {
		return err
	}
	m.loadMu.Lock()
	defer m.loadMu.Unlock()
	if m.loaded {
		return nil
	}
	messages := make([]*memory.Message, 0, len(m.rows))
	for i, row := range m.rows {
		message, err := m.backend.exportMessage(row, uint32(i+1))
		if err != nil {
			return err
		}
		messages = append(messages, message)
	}
	m.Messages = messages
	m.loaded = true
	m.count = uint32(len(messages))
	m.unseen = 0
	for _, message := range messages {
		if !hasFlag(message.Flags, imap.SeenFlag) {
			m.unseen++
		}
	}
	m.countLoaded = true
	return nil
}

func (m *mailbox) loadRows() error {
	m.loadMu.Lock()
	defer m.loadMu.Unlock()
	if m.rowsLoaded {
		return nil
	}
	rows, total, err := m.backend.listMailbox(mailsvc.Mailbox{ID: m.id, Name: m.name, Folder: true})
	if err != nil {
		return err
	}
	m.rows = rows
	m.count = uint32(total)
	m.rowsLoaded = true
	return nil
}

func (m *mailbox) Name() string { return m.name }
func (m *mailbox) Info() (*imap.MailboxInfo, error) {
	return &imap.MailboxInfo{Name: m.name, Delimiter: "/"}, nil
}

func (m *mailbox) Status(items []imap.StatusItem) (*imap.MailboxStatus, error) {
	slog.DebugContext(m.backend.ctx, "local IMAP client requested mailbox status")
	m.loadMu.Lock()
	loaded := m.loaded
	m.loadMu.Unlock()
	if !loaded {
		if err := m.loadCounts(); err != nil {
			return nil, err
		}
	}
	m.loadMu.Lock()
	defer m.loadMu.Unlock()
	status := imap.NewMailboxStatus(m.name, items)
	status.ReadOnly = true
	status.UidValidity = m.uidValidity
	status.Messages = m.count
	status.UidNext = m.count + 1
	status.Flags = []string{imap.SeenFlag, imap.FlaggedFlag}
	status.PermanentFlags = []string{}
	for i, msg := range m.Messages {
		if !hasFlag(msg.Flags, imap.SeenFlag) {
			status.Unseen++
			if status.UnseenSeqNum == 0 {
				status.UnseenSeqNum = uint32(i + 1)
			}
		}
	}
	if !m.loaded {
		status.Unseen = m.unseen
	}
	return status, nil
}

func (m *mailbox) loadCounts() error {
	m.loadMu.Lock()
	defer m.loadMu.Unlock()
	if m.countLoaded {
		return nil
	}
	// Counts resolves its folder argument as a user-facing name. Built-in
	// Proton folder IDs such as Inbox's "0" are query IDs, not names.
	counts, err := m.backend.mail.Counts(m.backend.ctx, false, m.name)
	if err != nil {
		return err
	}
	for _, count := range counts {
		m.count, m.unseen = uint32(count.Total), uint32(count.Unread)
		break
	}
	m.countLoaded = true
	return nil
}

func (m *mailbox) ListMessages(_ bool, seqSet *imap.SeqSet, items []imap.FetchItem, ch chan<- *imap.Message) error {
	defer close(ch)
	started := time.Now()
	slog.DebugContext(m.backend.ctx, "local IMAP client requested messages")
	metadataOnly := metadataOnlyFetch(items)
	completion := "local IMAP message-body fetch completed"
	if metadataOnly {
		completion = "local IMAP metadata fetch completed"
	}
	const pageSize = 150
	const exportWorkers = 4
	var total int
	var exported int
	for page := 0; ; page++ {
		rows, count, err := m.backend.mail.List(m.backend.ctx, mailsvc.ListOptions{
			Folder: m.id, Page: page, PageSize: pageSize,
		})
		if err != nil {
			return fmt.Errorf("list mailbox %q: %w", m.name, err)
		}
		if page == 0 {
			total = count
			slog.DebugContext(m.backend.ctx, "local IMAP message index received from Proton")
			m.loadMu.Lock()
			if !m.countLoaded {
				m.count = uint32(total)
				m.countLoaded = true
			}
			m.loadMu.Unlock()
		}
		if len(rows) == 0 {
			break
		}
		// Proton returns newest first. Assign IMAP sequence numbers from the
		// oldest end, while processing each page newest-first so Thunderbird gets
		// recent mail as soon as the first page is exported.
		for i := 0; i < len(rows); i += exportWorkers {
			end := i + exportWorkers
			if end > len(rows) {
				end = len(rows)
			}
			type result struct {
				message *imap.Message
				err     error
			}
			results := make([]result, end-i)
			var workers sync.WaitGroup
			for j := i; j < end; j++ {
				j := j
				workers.Add(1)
				go func() {
					defer workers.Done()
					row := rows[j]
					offset := page*pageSize + j
					if offset >= total {
						return
					}
					uidNum := uint32(total - offset)
					if !containsSeqSet(seqSet, uidNum, uint32(total)) {
						return
					}
					var message *memory.Message
					if metadataOnly {
						message = m.backend.metadataMessage(row, uidNum)
					} else {
						var err error
						message, err = m.backend.exportMessage(row, uidNum)
						if err != nil {
							results[j-i].err = err
							return
						}
					}
					results[j-i].message, results[j-i].err = message.Fetch(uidNum, items)
				}()
			}
			workers.Wait()
			for _, result := range results {
				if result.err != nil {
					return fmt.Errorf("export message for mailbox %q: %w", m.name, result.err)
				}
				if result.message == nil {
					continue
				}
				ch <- result.message
				exported++
			}
		}
		if page*pageSize+len(rows) >= total {
			break
		}
	}
	slog.DebugContext(m.backend.ctx, completion,
		"count", exported, "duration_ms", time.Since(started).Milliseconds())
	return nil
}

func containsSeqSet(set *imap.SeqSet, number, maximum uint32) bool {
	if set.Contains(number) {
		return true
	}
	// go-imap represents a standalone "*" as the zero range. Resolve it to
	// the largest sequence number or UID in this mailbox snapshot.
	for _, seq := range set.Set {
		if seq.Start == 0 && seq.Stop == 0 && number == maximum {
			return true
		}
	}
	return false
}

func (m *mailbox) SearchMessages(uid bool, criteria *imap.SearchCriteria) ([]uint32, error) {
	if err := m.loadMessages(); err != nil {
		return nil, fmt.Errorf("read mailbox %q: %w", m.name, err)
	}
	return m.Mailbox.SearchMessages(uid, criteria)
}

func (m *mailbox) UpdateMessagesFlags(bool, *imap.SeqSet, imap.FlagsOp, []string) error {
	return errors.New("mailboxes are read-only")
}

func (m *mailbox) CreateMessage(_ []string, _ time.Time, message imap.Literal) error {
	if strings.EqualFold(m.name, "sent") {
		// Proton's SMTP send operation already files the delivered message in
		// Sent. Thunderbird appends its own sent copy after SMTP succeeds; consume
		// that duplicate without adding another copy to Proton.
		_, err := io.Copy(io.Discard, message)
		return err
	}
	return errors.New("mailboxes are read-only")
}
func (m *mailbox) CopyMessages(bool, *imap.SeqSet, string) error {
	return errors.New("mailboxes are read-only")
}

// MoveMessages files messages in Proton's destination folder. Proton's label
// endpoint performs folder moves, matching the CLI's `mail messages move`.
func (m *mailbox) MoveMessages(_ bool, seqSet *imap.SeqSet, destName string) error {
	if err := m.backend.loadMailboxes(); err != nil {
		return fmt.Errorf("list mailboxes: %w", err)
	}
	m.backend.mu.Lock()
	dest, ok := m.backend.boxes[destName]
	if !ok && strings.EqualFold(destName, "INBOX") {
		dest, ok = m.backend.boxes["INBOX"]
	}
	m.backend.mu.Unlock()
	if !ok {
		return backend.ErrNoSuchMailbox
	}
	if dest.id == m.id {
		return nil
	}
	if err := m.loadRows(); err != nil {
		return fmt.Errorf("read mailbox %q: %w", m.name, err)
	}
	ids := selectedMessageIDs(m.rows, seqSet)
	if len(ids) == 0 {
		return nil
	}
	refused, err := m.backend.mail.Label(m.backend.ctx, ids, dest.id)
	if err != nil {
		return fmt.Errorf("move messages: %w", err)
	}
	refusedIDs := make(map[string]bool, len(refused))
	for _, r := range refused {
		refusedIDs[r.ID] = true
	}
	moved := make(map[string]bool, max(0, len(ids)-len(refusedIDs)))
	for _, id := range ids {
		if !refusedIDs[id] {
			moved[id] = true
		}
	}
	if len(moved) > 0 {
		m.removeMessages(moved)
		dest.resetSnapshot()
	}
	if len(refused) > 0 {
		return fmt.Errorf("Proton refused to move %d messages", len(refused))
	}
	slog.DebugContext(m.backend.ctx, "local IMAP messages moved to Proton folder", "count", len(moved))
	return nil
}

func selectedMessageIDs(rows []mailsvc.Message, seqSet *imap.SeqSet) []string {
	if seqSet == nil {
		return nil
	}
	ids := make([]string, 0)
	maximum := uint32(len(rows))
	for i, row := range rows {
		number := uint32(i + 1)
		if !containsSeqSet(seqSet, number, maximum) {
			continue
		}
		// This bridge currently assigns the same monotonically increasing values
		// to sequence numbers and UIDs in a mailbox snapshot.
		ids = append(ids, row.ID)
	}
	return ids
}

func (m *mailbox) removeMessages(ids map[string]bool) {
	m.loadMu.Lock()
	defer m.loadMu.Unlock()
	rows := make([]mailsvc.Message, 0, len(m.rows))
	var messages []*memory.Message
	if m.loaded {
		messages = make([]*memory.Message, 0, len(m.Messages))
	}
	for i, row := range m.rows {
		if ids[row.ID] {
			continue
		}
		rows = append(rows, row)
		if m.loaded && i < len(m.Messages) {
			messages = append(messages, m.Messages[i])
		}
	}
	m.rows = rows
	if m.loaded {
		m.Messages = messages
	}
	m.count = uint32(len(rows))
	m.countLoaded = true
	if m.loaded {
		m.unseen = 0
		for _, message := range messages {
			if !hasFlag(message.Flags, imap.SeenFlag) {
				m.unseen++
			}
		}
	}
}

func (m *mailbox) resetSnapshot() {
	m.loadMu.Lock()
	defer m.loadMu.Unlock()
	m.rows = nil
	m.rowsLoaded = false
	m.loaded = false
	m.Messages = nil
	m.count = 0
	m.unseen = 0
	m.countLoaded = false
}

func (m *mailbox) Expunge() error { return errors.New("mailboxes are read-only") }

func hasFlag(flags []string, want string) bool {
	for _, flag := range flags {
		if strings.EqualFold(flag, want) {
			return true
		}
	}
	return false
}

var _ backend.Backend = (*Backend)(nil)
var _ backend.User = (*user)(nil)
var _ backend.Mailbox = (*mailbox)(nil)
var _ backend.MoveMailbox = (*mailbox)(nil)
