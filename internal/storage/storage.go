package storage

import "marznode/internal/models"

type Storage interface {
	ListUsers() []models.User
	GetUser(id uint32) (models.User, bool)
	UpdateUserInbounds(user models.User, inbounds []models.Inbound)
	RemoveUser(id uint32)
	FlushUsers()

	ListInbounds(tags []string) []models.Inbound
	RegisterInbound(inbound models.Inbound)
	RemoveInbound(tag string)
	UnregisterInbound(tag string)
	ListInboundUsers(tag string) []models.User
}
