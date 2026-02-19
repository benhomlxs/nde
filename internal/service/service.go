package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"marznode/internal/backend"
	"marznode/internal/gen/servicepb"
	"marznode/internal/models"
	"marznode/internal/storage"
)

type Service struct {
	servicepb.UnimplementedMarzServiceServer

	store    storage.Storage
	backends map[string]backend.Backend

	lockMu    sync.Mutex
	userLocks map[uint32]*sync.Mutex
}

const repopulateMarkerUsername = "__marznode_repopulate__"

func New(store storage.Storage, backends map[string]backend.Backend) *Service {
	return &Service{
		store:     store,
		backends:  backends,
		userLocks: make(map[uint32]*sync.Mutex),
	}
}

func (s *Service) lockUser(uid uint32) func() {
	s.lockMu.Lock()
	lk, ok := s.userLocks[uid]
	if !ok {
		lk = &sync.Mutex{}
		s.userLocks[uid] = lk
	}
	s.lockMu.Unlock()

	lk.Lock()
	return lk.Unlock
}

func (s *Service) backendByTag(tag string) (backend.Backend, error) {
	for _, b := range s.backends {
		if b.ContainsTag(tag) {
			return b, nil
		}
	}
	return nil, fmt.Errorf("no backend for inbound tag %q", tag)
}

func (s *Service) addUser(ctx context.Context, user models.User, inbounds []models.Inbound) error {
	for _, inb := range inbounds {
		b, err := s.backendByTag(inb.Tag)
		if err != nil {
			return err
		}
		if err := b.AddUser(ctx, user, inb); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) removeUser(ctx context.Context, user models.User, inbounds []models.Inbound) error {
	for _, inb := range inbounds {
		b, err := s.backendByTag(inb.Tag)
		if err != nil {
			return err
		}
		if err := b.RemoveUser(ctx, user, inb); err != nil {
			return err
		}
	}
	return nil
}

func pbUserToModel(pb *servicepb.User) models.User {
	if pb == nil {
		return models.User{}
	}
	return models.User{
		ID:       pb.GetId(),
		Username: pb.GetUsername(),
		Key:      pb.GetKey(),
	}
}

func inboundTags(inbounds []*servicepb.Inbound) []string {
	out := make([]string, 0, len(inbounds))
	for _, inb := range inbounds {
		out = append(out, inb.GetTag())
	}
	return out
}

func (s *Service) resolveInbounds(tags []string) []models.Inbound {
	unique := make([]string, 0, len(tags))
	seen := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		if tag == "" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		unique = append(unique, tag)
	}
	return s.store.ListInbounds(unique)
}

func isRepopulateMarker(msg *servicepb.UserData) bool {
	if msg == nil || msg.GetUser() == nil {
		return false
	}
	// User IDs are expected to be >= 1 in normal operation, so ID=0 is safe as a control marker.
	return msg.GetUser().GetId() == 0 && msg.GetUser().GetUsername() == repopulateMarkerUsername
}

func (s *Service) applyUserData(ctx context.Context, data *servicepb.UserData) error {
	user := pbUserToModel(data.GetUser())
	if user.ID == 0 || user.Username == "" {
		return nil
	}
	unlock := s.lockUser(user.ID)
	defer unlock()

	stored, hasStored := s.store.GetUser(user.ID)
	reqTags := inboundTags(data.GetInbounds())

	switch {
	case !hasStored && len(reqTags) > 0:
		additions := s.resolveInbounds(reqTags)
		if err := s.addUser(ctx, user, additions); err != nil {
			return err
		}
		s.store.UpdateUserInbounds(user, additions)
		return nil
	case hasStored && len(reqTags) == 0:
		if err := s.removeUser(ctx, stored, stored.Inbounds); err != nil {
			return err
		}
		s.store.RemoveUser(user.ID)
		return nil
	case !hasStored && len(reqTags) == 0:
		return nil
	}

	current := make(map[string]struct{}, len(stored.Inbounds))
	for _, inb := range stored.Inbounds {
		current[inb.Tag] = struct{}{}
	}
	desired := make(map[string]struct{}, len(reqTags))
	for _, tag := range reqTags {
		desired[tag] = struct{}{}
	}

	addedTags := make([]string, 0)
	removedTags := make([]string, 0)
	for tag := range desired {
		if _, ok := current[tag]; !ok {
			addedTags = append(addedTags, tag)
		}
	}
	for tag := range current {
		if _, ok := desired[tag]; !ok {
			removedTags = append(removedTags, tag)
		}
	}

	newInbounds := s.resolveInbounds(reqTags)
	addedInbounds := s.resolveInbounds(addedTags)
	removedInbounds := s.resolveInbounds(removedTags)

	if err := s.removeUser(ctx, stored, removedInbounds); err != nil {
		return err
	}
	if err := s.addUser(ctx, stored, addedInbounds); err != nil {
		return err
	}
	s.store.UpdateUserInbounds(stored, newInbounds)
	return nil
}

func (s *Service) SyncUsers(stream grpc.ClientStreamingServer[servicepb.UserData, servicepb.Empty]) error {
	ctx := stream.Context()
	repopulateMode := false
	first := true
	keep := map[uint32]struct{}{}
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			if repopulateMode {
				existing := s.store.ListUsers()
				for _, user := range existing {
					if _, ok := keep[user.ID]; ok {
						continue
					}
					if err := s.removeUser(ctx, user, user.Inbounds); err != nil {
						log.Printf("repopulate stream cleanup skipped uid=%d username=%q error=%v", user.ID, user.Username, err)
						continue
					}
					s.store.RemoveUser(user.ID)
				}
			}
			return stream.SendAndClose(&servicepb.Empty{})
		}
		if err != nil {
			return err
		}

		if first {
			first = false
			if isRepopulateMarker(msg) {
				repopulateMode = true
				continue
			}
		}

		if repopulateMode {
			u := msg.GetUser()
			if u != nil && u.GetId() != 0 {
				keep[u.GetId()] = struct{}{}
			}
		}

		uCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err = s.applyUserData(uCtx, msg)
		cancel()
		if err != nil {
			u := msg.GetUser()
			log.Printf("sync user skipped uid=%d username=%q error=%v", u.GetId(), u.GetUsername(), err)
			continue
		}
	}
}

func (s *Service) RepopulateUsers(ctx context.Context, req *servicepb.UsersData) (*servicepb.Empty, error) {
	for _, userData := range req.GetUsersData() {
		uCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := s.applyUserData(uCtx, userData)
		cancel()
		if err != nil {
			u := userData.GetUser()
			log.Printf("repopulate user skipped uid=%d username=%q error=%v", u.GetId(), u.GetUsername(), err)
			continue
		}
	}

	existing := s.store.ListUsers()
	keep := make(map[uint32]struct{}, len(req.GetUsersData()))
	for _, userData := range req.GetUsersData() {
		keep[userData.GetUser().GetId()] = struct{}{}
	}
	for _, user := range existing {
		if _, ok := keep[user.ID]; ok {
			continue
		}
		if err := s.removeUser(ctx, user, user.Inbounds); err != nil {
			log.Printf("repopulate cleanup skipped uid=%d username=%q error=%v", user.ID, user.Username, err)
			continue
		}
		s.store.RemoveUser(user.ID)
	}
	return &servicepb.Empty{}, nil
}

func (s *Service) FetchBackends(ctx context.Context, _ *servicepb.Empty) (*servicepb.BackendsResponse, error) {
	_ = ctx
	out := &servicepb.BackendsResponse{
		Backends: make([]*servicepb.Backend, 0, len(s.backends)),
	}
	for name, b := range s.backends {
		inbounds := b.ListInbounds()
		pbInbounds := make([]*servicepb.Inbound, 0, len(inbounds))
		for _, inb := range inbounds {
			raw, _ := json.Marshal(inb.Config)
			rawStr := string(raw)
			pbInbounds = append(pbInbounds, &servicepb.Inbound{
				Tag:    inb.Tag,
				Config: &rawStr,
			})
		}
		out.Backends = append(out.Backends, &servicepb.Backend{
			Name:     name,
			Type:     strPtr(b.Type()),
			Version:  strPtr(b.Version()),
			Inbounds: pbInbounds,
		})
	}
	return out, nil
}

func (s *Service) FetchUsersStats(ctx context.Context, _ *servicepb.Empty) (*servicepb.UsersStats, error) {
	all := map[uint32]uint64{}
	for _, b := range s.backends {
		stats, err := b.GetUsages(ctx, true)
		if err != nil {
			continue
		}
		for uid, usage := range stats {
			all[uid] += usage
		}
	}
	out := &servicepb.UsersStats{UsersStats: make([]*servicepb.UsersStats_UserStats, 0, len(all))}
	for uid, usage := range all {
		out.UsersStats = append(out.UsersStats, &servicepb.UsersStats_UserStats{
			Uid:   uid,
			Usage: usage,
		})
	}
	return out, nil
}

func (s *Service) StreamBackendLogs(req *servicepb.BackendLogsRequest, stream grpc.ServerStreamingServer[servicepb.LogLine]) error {
	b, ok := s.backends[req.GetBackendName()]
	if !ok {
		return status.Error(codes.NotFound, "backend not found")
	}
	logs, err := b.LogStream(stream.Context(), req.GetIncludeBuffer())
	if err != nil {
		return status.Errorf(codes.Unavailable, "log stream failed: %v", err)
	}
	for line := range logs {
		if err := stream.Send(&servicepb.LogLine{Line: line}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) FetchBackendConfig(ctx context.Context, req *servicepb.Backend) (*servicepb.BackendConfig, error) {
	b, ok := s.backends[req.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "backend not found")
	}
	cfg, err := b.GetConfig(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "fetch config failed: %v", err)
	}
	return &servicepb.BackendConfig{
		Configuration: cfg,
		ConfigFormat:  servicepb.ConfigFormat(b.ConfigFormat()),
	}, nil
}

func (s *Service) RestartBackend(ctx context.Context, req *servicepb.RestartBackendRequest) (*servicepb.Empty, error) {
	b, ok := s.backends[req.GetBackendName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "backend not found")
	}
	cfg := ""
	if req.GetConfig() != nil {
		cfg = req.GetConfig().GetConfiguration()
	}
	if err := b.Restart(ctx, cfg); err != nil {
		return nil, status.Errorf(codes.Unavailable, "backend restart failed: %v", err)
	}
	return &servicepb.Empty{}, nil
}

func (s *Service) GetBackendStats(ctx context.Context, req *servicepb.Backend) (*servicepb.BackendStats, error) {
	_ = ctx
	b, ok := s.backends[req.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "backend not found")
	}
	return &servicepb.BackendStats{Running: b.Running()}, nil
}

func strPtr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
