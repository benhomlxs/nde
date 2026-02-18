package models

type User struct {
	ID       uint32
	Username string
	Key      string
	Inbounds []Inbound
}

type Inbound struct {
	Tag      string
	Protocol string
	Config   map[string]any
}

func CloneInbounds(inbounds []Inbound) []Inbound {
	out := make([]Inbound, len(inbounds))
	copy(out, inbounds)
	return out
}
