package storage

import (
	"sync"

	"marznode/internal/models"
)

type Memory struct {
	mu       sync.RWMutex
	users    map[uint32]models.User
	inbounds map[string]models.Inbound
}

func NewMemory() *Memory {
	return &Memory{
		users:    make(map[uint32]models.User),
		inbounds: make(map[string]models.Inbound),
	}
}

func (m *Memory) ListUsers() []models.User {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]models.User, 0, len(m.users))
	for _, u := range m.users {
		u.Inbounds = models.CloneInbounds(u.Inbounds)
		out = append(out, u)
	}
	return out
}

func (m *Memory) GetUser(id uint32) (models.User, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	u, ok := m.users[id]
	if !ok {
		return models.User{}, false
	}
	u.Inbounds = models.CloneInbounds(u.Inbounds)
	return u, true
}

func (m *Memory) UpdateUserInbounds(user models.User, inbounds []models.Inbound) {
	m.mu.Lock()
	defer m.mu.Unlock()

	user.Inbounds = models.CloneInbounds(inbounds)
	m.users[user.ID] = user
}

func (m *Memory) RemoveUser(id uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.users, id)
}

func (m *Memory) FlushUsers() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users = make(map[uint32]models.User)
}

func (m *Memory) ListInbounds(tags []string) []models.Inbound {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(tags) == 0 {
		out := make([]models.Inbound, 0, len(m.inbounds))
		for _, inb := range m.inbounds {
			out = append(out, inb)
		}
		return out
	}

	out := make([]models.Inbound, 0, len(tags))
	for _, tag := range tags {
		inb, ok := m.inbounds[tag]
		if ok {
			out = append(out, inb)
		}
	}
	return out
}

func (m *Memory) RegisterInbound(inbound models.Inbound) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inbounds[inbound.Tag] = inbound
}

func (m *Memory) RemoveInbound(tag string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inbounds, tag)

	for id, user := range m.users {
		next := make([]models.Inbound, 0, len(user.Inbounds))
		for _, inb := range user.Inbounds {
			if inb.Tag != tag {
				next = append(next, inb)
			}
		}
		user.Inbounds = next
		m.users[id] = user
	}
}

func (m *Memory) UnregisterInbound(tag string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inbounds, tag)
}

func (m *Memory) ListInboundUsers(tag string) []models.User {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]models.User, 0)
	for _, user := range m.users {
		for _, inb := range user.Inbounds {
			if inb.Tag == tag {
				user.Inbounds = models.CloneInbounds(user.Inbounds)
				out = append(out, user)
				break
			}
		}
	}
	return out
}
