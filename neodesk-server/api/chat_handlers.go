// Chat REST API handlers — message persistence, contacts, groups.
//
// Endpoints:
//   GET  /api/chat/history/:conversation_id  — get message history
//   POST /api/chat/messages                  — save a new message
//   POST /api/chat/read                      — mark messages as read
//   GET  /api/chat/unread/:device_id         — get unread count
//   GET  /api/chat/contacts/:device_id       — get chat contacts (operators, devices of its groups)
//   POST /api/chat/groups                    — create a group
//   GET  /api/chat/groups/:member_id         — list groups for member
//   PUT  /api/chat/groups/:id                — update group
//   DELETE /api/chat/groups/:id              — delete group

package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/neodeskapp/neodesk/neodesk-server/auth"
	"github.com/neodeskapp/neodesk/neodesk-server/config"
	"github.com/neodeskapp/neodesk/neodesk-server/db"
	"github.com/neodeskapp/neodesk/neodesk-server/events"
)

// handleChatHistory returns message history for a conversation.
// GET /api/chat/history/{conversation_id}?limit=100&before=123
func (s *Server) handleChatHistory(w http.ResponseWriter, r *http.Request) {
	convID := strings.TrimPrefix(r.URL.Path, "/api/chat/history/")
	if convID == "" {
		http.Error(w, `{"error":"conversation_id required"}`, http.StatusBadRequest)
		return
	}

	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	var msgs []*db.ChatMessage
	var err error

	if beforeStr := r.URL.Query().Get("before"); beforeStr != "" {
		beforeID, _ := strconv.ParseInt(beforeStr, 10, 64)
		if beforeID > 0 {
			msgs, err = s.db.GetChatHistoryBefore(convID, beforeID, limit)
		} else {
			msgs, err = s.db.GetChatHistory(convID, limit)
		}
	} else {
		msgs, err = s.db.GetChatHistory(convID, limit)
	}

	if err != nil {
		log.Printf("[chat] GetChatHistory error: %v", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}

	if msgs == nil {
		msgs = []*db.ChatMessage{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"conversation_id": convID,
		"messages":        msgs,
	})
}

// handleChatSendMessage saves a new chat message.
// POST /api/chat/messages
// Body: { "conversation_id": "...", "from_id": "...", "from_name": "...", "to_id": "...", "text": "..." }
func (s *Server) handleChatSendMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ConversationID string `json:"conversation_id"`
		FromID         string `json:"from_id"`
		FromName       string `json:"from_name"`
		ToID           string `json:"to_id"`
		Text           string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	// The console relays messages of its signed-in operators and names
	// them; any other caller posts as itself.
	if !arrivedInternally(r) {
		body.FromID = getUsernameFromCtx(r)
		body.FromName = body.FromID
	}

	if body.ConversationID == "" || body.FromID == "" || body.Text == "" {
		http.Error(w, `{"error":"conversation_id, from_id, and text required"}`, http.StatusBadRequest)
		return
	}

	// Truncate text to 4096 chars
	if len(body.Text) > 4096 {
		body.Text = body.Text[:4096]
	}

	msg := &db.ChatMessage{
		ConversationID: body.ConversationID,
		FromID:         body.FromID,
		FromName:       body.FromName,
		ToID:           body.ToID,
		Text:           body.Text,
		CreatedAt:      time.Now(),
	}

	id, err := s.db.SaveChatMessage(msg)
	if err != nil {
		log.Printf("[chat] SaveChatMessage error: %v", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}

	msg.ID = id

	// Notify subscribers (e.g. the Node.js panel) so they can fan the message
	// out to operator browsers in real time.
	if s.eventBus != nil {
		s.eventBus.Publish(events.Event{
			Type: "chat_message",
			Data: map[string]string{
				"id":              strconv.FormatInt(id, 10),
				"conversation_id": msg.ConversationID,
				"from_id":         msg.FromID,
				"from_name":       msg.FromName,
				"to_id":           msg.ToID,
				"text":            msg.Text,
			},
		})
	}

	// Deliver to the target device over CDAP if it is connected. The message is
	// already persisted; CDAP delivery is best-effort for online agents.
	if body.ToID != "" {
		if _, err := s.cdapChat(r.Context(), body.ToID, body.FromID, body.FromName, body.Text); err != nil {
			log.Printf("[chat] CDAP delivery to %s failed: %v", body.ToID, err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(msg)
}

// handleChatMarkRead marks messages as read.
// POST /api/chat/read
// Body: { "conversation_id": "...", "reader_id": "..." }
func (s *Server) handleChatMarkRead(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ConversationID string `json:"conversation_id"`
		ReaderID       string `json:"reader_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}

	// The console marks messages read for its signed-in operators and
	// devices; any other caller marks them read as itself.
	if !arrivedInternally(r) {
		body.ReaderID = getUsernameFromCtx(r)
	}

	if body.ConversationID == "" || body.ReaderID == "" {
		http.Error(w, `{"error":"conversation_id and reader_id required"}`, http.StatusBadRequest)
		return
	}
	// A group conversation must be an existing group; a device's
	// conversation must be a device in the caller's scope.
	if strings.HasPrefix(body.ConversationID, chatGroupPrefix) {
		if group, err := s.db.GetChatGroup(body.ConversationID); err != nil || group == nil {
			http.Error(w, `{"error":"group not found"}`, http.StatusNotFound)
			return
		}
	} else if !s.deviceInScope(w, r, body.ConversationID) {
		return
	}

	if err := s.db.MarkChatRead(body.ConversationID, body.ReaderID); err != nil {
		log.Printf("[chat] MarkChatRead error: %v", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// handleChatUnread returns unread count for a device.
// GET /api/chat/unread/{device_id}
func (s *Server) handleChatUnread(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.TrimPrefix(r.URL.Path, "/api/chat/unread/")
	if deviceID == "" {
		http.Error(w, `{"error":"device_id required"}`, http.StatusBadRequest)
		return
	}

	count, err := s.db.GetUnreadCount(deviceID)
	if err != nil {
		log.Printf("[chat] GetUnreadCount error: %v", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"device_id": deviceID,
		"unread":    count,
	})
}

// chatPanelID is the id the console asks for the contacts of its chat
// inbox with. The console filters that list by the account's device scope.
const chatPanelID = "panel"

// chatGroupMates returns the ids of the members of every chat group that
// has deviceID among its members.
func (s *Server) chatGroupMates(deviceID string) (map[string]bool, error) {
	groups, err := s.db.ListChatGroups(deviceID)
	if err != nil {
		return nil, err
	}
	mates := map[string]bool{}
	for _, g := range groups {
		members := strings.Split(g.Members, ",")
		for i := range members {
			members[i] = strings.TrimSpace(members[i])
		}
		// The database matches the id anywhere in the list
		if !slices.Contains(members, deviceID) {
			continue
		}
		for _, member := range members {
			mates[member] = true
		}
	}
	return mates, nil
}

// chatPeerOnline says whether a device is online by the device list's
// rule (GET /api/peers live_online): a live signal entry, a heartbeat
// within the registration timeout, or a CDAP or MeshCentral connection.
// The status stored in the database is not enough: it stays ONLINE for a
// device that went away.
func (s *Server) chatPeerOnline(p *db.Peer) bool {
	if s.blockedReason(p.ID) != "" {
		return false
	}
	if s.peers != nil && s.peers.IsOnline(p.ID, config.RegTimeout) {
		return true
	}
	if !p.LastOnline.IsZero() && time.Since(p.LastOnline) <= config.RegTimeout {
		return true
	}
	ctx := context.Background()
	return s.deviceConnectedAnywhere(ctx, "cdap", p.ID) || s.deviceConnectedAnywhere(ctx, "mesh", p.ID)
}

// handleChatContacts returns the chat contacts of a device: the operators
// and the devices it shares a chat group with. The panel's inbox gets every
// device.
// GET /api/chat/contacts/{device_id}
func (s *Server) handleChatContacts(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.TrimPrefix(r.URL.Path, "/api/chat/contacts/")

	peers, err := s.db.ListPeers(false)
	if err != nil {
		log.Printf("[chat] ListPeers error: %v", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}
	var mates map[string]bool
	if deviceID != chatPanelID {
		if mates, err = s.chatGroupMates(deviceID); err != nil {
			log.Printf("[chat] ListChatGroups error: %v", err)
			http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
			return
		}
	}

	type contact struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Hostname    string `json:"hostname"`
		Online      bool   `json:"online"`
		LastSeen    int64  `json:"last_seen"`
		Unread      int    `json:"unread"`
		AvatarColor string `json:"avatar_color"`
		Role        string `json:"role"` // "device" or "operator"
	}

	// The unread messages the asking device has, by contact: a device's
	// under its id, an operator's (from "operator") under
	// operator:<name>
	unread := map[string]int{}
	if counts, err := s.db.GetUnreadBySender(deviceID); err == nil {
		for _, c := range counts {
			sender := c.FromID
			if sender == "operator" {
				sender = "operator:" + c.FromName
			}
			unread[sender] += c.Count
		}
	} else {
		log.Printf("[chat] GetUnreadBySender error: %v", err)
	}

	var contacts []contact
	for _, p := range peers {
		if p.ID == deviceID || (mates != nil && !mates[p.ID]) {
			continue
		}
		c := contact{
			ID:          p.ID,
			Name:        p.Hostname,
			Hostname:    p.Hostname,
			Online:      s.chatPeerOnline(p),
			LastSeen:    p.LastOnline.UnixMilli(),
			AvatarColor: deviceColor(p.ID),
			Role:        "device",
		}
		if c.Name == "" {
			c.Name = p.ID
		}
		c.Unread = unread[p.ID]
		contacts = append(contacts, c)
	}

	// Add operators as contacts
	users, err := s.db.ListUsers()
	if err == nil {
		for _, u := range users {
			if auth.RoleHasPermission(u.Role, auth.PermChatSupport) || auth.RoleHasPermission(u.Role, auth.PermChatTeam) {
				contacts = append(contacts, contact{
					ID:          "operator:" + u.Username,
					Name:        u.Username,
					Hostname:    "Console",
					Online:      true, // operators are always "available" from device perspective
					Unread:      unread["operator:"+u.Username],
					AvatarColor: deviceColor(u.Username),
					Role:        "operator",
				})
			}
		}
	}

	if contacts == nil {
		contacts = []contact{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"contacts": contacts,
	})
}

// handleChatCreateGroup creates a new chat group.
// POST /api/chat/groups
// Body: { "name": "...", "members": ["id1","id2"], "created_by": "..." }
func (s *Server) handleChatCreateGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      string   `json:"name"`
		Members   []string `json:"members"`
		CreatedBy string   `json:"created_by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if body.Name == "" || len(body.Members) == 0 {
		http.Error(w, `{"error":"name and members required"}`, http.StatusBadRequest)
		return
	}

	// The creator decides who may delete the group, so a caller other
	// than the console creates it as itself.
	if !arrivedInternally(r) {
		body.CreatedBy = getUsernameFromCtx(r)
	}

	g := &db.ChatGroup{
		ID:        chatGroupPrefix + uuid.New().String()[:8],
		Name:      body.Name,
		Members:   strings.Join(body.Members, ","),
		CreatedBy: body.CreatedBy,
		CreatedAt: time.Now(),
	}

	if err := s.db.CreateChatGroup(g); err != nil {
		log.Printf("[chat] CreateChatGroup error: %v", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(g)
}

// handleChatListGroups lists groups for a member.
// GET /api/chat/groups/{member_id}
func (s *Server) handleChatListGroups(w http.ResponseWriter, r *http.Request) {
	memberID := strings.TrimPrefix(r.URL.Path, "/api/chat/groups/")
	if memberID == "" {
		http.Error(w, `{"error":"member_id required"}`, http.StatusBadRequest)
		return
	}

	groups, err := s.db.ListChatGroups(memberID)
	if err != nil {
		log.Printf("[chat] ListChatGroups error: %v", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}

	if groups == nil {
		groups = []*db.ChatGroup{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"groups": groups,
	})
}

// handleChatUpdateGroup updates a chat group.
// PUT /api/chat/groups/{id}
func (s *Server) handleChatUpdateGroup(w http.ResponseWriter, r *http.Request) {
	groupID := strings.TrimPrefix(r.URL.Path, "/api/chat/groups/")
	if groupID == "" {
		http.Error(w, `{"error":"group id required"}`, http.StatusBadRequest)
		return
	}

	var body struct {
		Name    string   `json:"name"`
		Members []string `json:"members"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	// Like delete: only an existing group, by its creator or an admin.
	group, err := s.db.GetChatGroup(groupID)
	if err != nil || group == nil {
		http.Error(w, `{"error":"group not found"}`, http.StatusNotFound)
		return
	}
	if !auth.IsSuperAdminRole(getRoleFromCtx(r)) && group.CreatedBy != actorFromCtx(r) {
		http.Error(w, `{"error":"only the group's creator or an admin may change it"}`, http.StatusForbidden)
		return
	}

	g := &db.ChatGroup{
		ID:      groupID,
		Name:    body.Name,
		Members: strings.Join(body.Members, ","),
	}

	if err := s.db.UpdateChatGroup(g); err != nil {
		log.Printf("[chat] UpdateChatGroup error: %v", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// handleChatDeleteGroup deletes a chat group and its history. Only an
// existing group is deleted, so a device conversation keeps its history,
// and only by its creator or a super admin.
// DELETE /api/chat/groups/{id}
func (s *Server) handleChatDeleteGroup(w http.ResponseWriter, r *http.Request) {
	groupID := strings.TrimPrefix(r.URL.Path, "/api/chat/groups/")
	if groupID == "" {
		http.Error(w, `{"error":"group id required"}`, http.StatusBadRequest)
		return
	}
	group, err := s.db.GetChatGroup(groupID)
	if err != nil || group == nil {
		http.Error(w, `{"error":"group not found"}`, http.StatusNotFound)
		return
	}
	if !auth.IsSuperAdminRole(getRoleFromCtx(r)) && group.CreatedBy != actorFromCtx(r) {
		http.Error(w, `{"error":"only the group's creator or an admin may delete it"}`, http.StatusForbidden)
		return
	}

	if err := s.db.DeleteChatGroup(groupID); err != nil {
		log.Printf("[chat] DeleteChatGroup error: %v", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}

	// Also delete group chat history
	_ = s.db.DeleteChatHistory(groupID)

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// chatGroupPrefix starts the id of every chat group.
const chatGroupPrefix = "group:"

// deviceColor generates a consistent HSL color from a device ID string.
func deviceColor(id string) string {
	var hash uint32
	for _, c := range id {
		hash = hash*31 + uint32(c)
	}
	hue := hash % 360
	return "hsl(" + strconv.Itoa(int(hue)) + ", 65%, 55%)"
}
