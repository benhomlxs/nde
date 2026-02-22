package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
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
	userLocks map[uint32]*userLockState

	syncUserApplyTimeout       time.Duration
	repopulateUserApplyTimeout time.Duration
	repopulateBulkThreshold    int
	repopulateChunkSize        int
	repopulateWorkers          int
	userApplyRetries           int
	userApplyRetryDelay        time.Duration
}

type userLockState struct {
	mu   sync.Mutex
	refs int
}

const repopulateMarkerUsername = "__marznode_repopulate__"
const defaultSyncUserApplyTimeout = 90 * time.Second
const defaultRepopulateUserApplyTimeout = 180 * time.Second
const defaultRepopulateBulkThreshold = 200
const defaultRepopulateChunkSize = 512
const defaultRepopulateWorkers = 16
const defaultUserApplyRetries = 3
const defaultUserApplyRetryDelay = 350 * time.Millisecond

func New(store storage.Storage, backends map[string]backend.Backend) *Service {
	s := &Service{
		store:                      store,
		backends:                   backends,
		userLocks:                  make(map[uint32]*userLockState),
		syncUserApplyTimeout:       defaultSyncUserApplyTimeout,
		repopulateUserApplyTimeout: defaultRepopulateUserApplyTimeout,
		repopulateBulkThreshold:    defaultRepopulateBulkThreshold,
		repopulateChunkSize:        defaultRepopulateChunkSize,
		repopulateWorkers:          defaultRepopulateWorkers,
		userApplyRetries:           defaultUserApplyRetries,
		userApplyRetryDelay:        defaultUserApplyRetryDelay,
	}
	if s.repopulateWorkers < 1 {
		s.repopulateWorkers = 1
	}
	if s.repopulateBulkThreshold < 1 {
		s.repopulateBulkThreshold = 1
	}
	if s.repopulateChunkSize < 1 {
		s.repopulateChunkSize = 1
	}
	if s.userApplyRetries < 1 {
		s.userApplyRetries = 1
	}
	if s.userApplyRetryDelay <= 0 {
		s.userApplyRetryDelay = 100 * time.Millisecond
	}
	return s
}

func serviceLogger() *slog.Logger {
	return slog.Default().With("component", "service")
}

func (s *Service) lockUser(uid uint32) func() {
	s.lockMu.Lock()
	lk, ok := s.userLocks[uid]
	if !ok {
		lk = &userLockState{}
		s.userLocks[uid] = lk
	}
	lk.refs++
	s.lockMu.Unlock()

	lk.mu.Lock()
	return func() {
		lk.mu.Unlock()

		s.lockMu.Lock()
		defer s.lockMu.Unlock()

		lk.refs--
		if lk.refs <= 0 {
			// Remove only if map still points to the same lock instance.
			if current, exists := s.userLocks[uid]; exists && current == lk {
				delete(s.userLocks, uid)
			}
		}
	}
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
	var errs []error
	for _, inb := range inbounds {
		b, err := s.backendByTag(inb.Tag)
		if err != nil {
			serviceLogger().Warn("add user skipped", "uid", user.ID, "username", user.Username, "tag", inb.Tag, "reason", err)
			continue
		}
		if err := b.AddUser(ctx, user, inb); err != nil {
			errs = append(errs, fmt.Errorf("tag=%s: %w", inb.Tag, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) removeUser(ctx context.Context, user models.User, inbounds []models.Inbound) error {
	var errs []error
	for _, inb := range inbounds {
		b, err := s.backendByTag(inb.Tag)
		if err != nil {
			serviceLogger().Warn("remove user skipped", "uid", user.ID, "username", user.Username, "tag", inb.Tag, "reason", err)
			continue
		}
		if err := b.RemoveUser(ctx, user, inb); err != nil {
			errs = append(errs, fmt.Errorf("tag=%s: %w", inb.Tag, err))
		}
	}
	return errors.Join(errs...)
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

func (s *Service) applyUserDataWithRetry(ctx context.Context, data *servicepb.UserData, perAttemptTimeout time.Duration) error {
	if s.userApplyRetries <= 1 {
		return s.applyUserDataWithTimeout(ctx, data, perAttemptTimeout)
	}

	var lastErr error
	for attempt := 1; attempt <= s.userApplyRetries; attempt++ {
		err := s.applyUserDataWithTimeout(ctx, data, perAttemptTimeout)
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
		if attempt == s.userApplyRetries {
			break
		}
		if err := waitWithContext(ctx, s.userApplyRetryDelay); err != nil {
			lastErr = errors.Join(lastErr, err)
			break
		}
	}
	return lastErr
}

func (s *Service) applyUserDataWithTimeout(ctx context.Context, data *servicepb.UserData, timeout time.Duration) error {
	if timeout <= 0 {
		return s.applyUserData(ctx, data)
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return s.applyUserData(attemptCtx, data)
}

func waitWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Service) SyncUsers(stream grpc.ClientStreamingServer[servicepb.UserData, servicepb.Empty]) error {
	ctx := stream.Context()
	logger := serviceLogger()
	repopulateMode := false
	first := true
	keep := map[uint32]struct{}{}
	repopulateData := make([]*servicepb.UserData, 0, s.repopulateChunkSize)
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			if repopulateMode {
				s.applyRepopulateChunk(ctx, repopulateData)
				s.cleanupRepopulateUsers(ctx, keep)
			}
			return stream.SendAndClose(&servicepb.Empty{})
		}
		if err != nil {
			if isExpectedStreamClose(err) {
				logger.Info("sync users stream closed by client", "reason", err)
				return nil
			}
			logger.Error("sync users stream receive failed", "error", err)
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
			repopulateData = append(repopulateData, msg)
			if len(repopulateData) >= s.repopulateChunkSize {
				s.applyRepopulateChunk(ctx, repopulateData)
				repopulateData = repopulateData[:0]
			}
			continue
		}

		err = s.applyUserDataWithRetry(ctx, msg, s.syncUserApplyTimeout)
		if err != nil {
			u := msg.GetUser()
			logger.Warn("sync user skipped", "uid", u.GetId(), "username", u.GetUsername(), "error", err)
			continue
		}
	}
}

func (s *Service) applyRepopulateChunk(ctx context.Context, repopulateData []*servicepb.UserData) {
	if len(repopulateData) == 0 {
		return
	}
	if len(repopulateData) >= s.repopulateBulkThreshold {
		s.applyRepopulateBulk(ctx, repopulateData)
		return
	}

	for _, msg := range repopulateData {
		err := s.applyUserDataWithRetry(ctx, msg, s.repopulateUserApplyTimeout)
		if err != nil {
			u := msg.GetUser()
			serviceLogger().Warn("repopulate user skipped", "uid", u.GetId(), "username", u.GetUsername(), "error", err)
		}
	}
}

func (s *Service) cleanupRepopulateUsers(ctx context.Context, keep map[uint32]struct{}) {
	existing := s.store.ListUsers()
	for _, user := range existing {
		if _, ok := keep[user.ID]; ok {
			continue
		}
		uCtx, cancel := context.WithTimeout(ctx, s.repopulateUserApplyTimeout)
		err := s.removeUser(uCtx, user, user.Inbounds)
		cancel()
		if err != nil {
			serviceLogger().Warn("repopulate stream cleanup skipped", "uid", user.ID, "username", user.Username, "error", err)
			continue
		}
		s.store.RemoveUser(user.ID)
	}
}

func (s *Service) applyRepopulateBulk(ctx context.Context, repopulateData []*servicepb.UserData) {
	type job struct {
		data *servicepb.UserData
	}

	workers := s.repopulateWorkers
	if workers < 1 {
		workers = 1
	}
	if workers > len(repopulateData) {
		workers = len(repopulateData)
	}
	if workers == 0 {
		return
	}

	jobs := make(chan job, workers*2)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				err := s.applyUserDataWithRetry(ctx, item.data, s.repopulateUserApplyTimeout)
				if err != nil {
					u := item.data.GetUser()
					serviceLogger().Warn("repopulate bulk user skipped", "uid", u.GetId(), "username", u.GetUsername(), "error", err)
				}
			}
		}()
	}

	for _, msg := range repopulateData {
		jobs <- job{data: msg}
	}
	close(jobs)
	wg.Wait()
}

func (s *Service) RepopulateUsers(ctx context.Context, req *servicepb.UsersData) (*servicepb.Empty, error) {
	for _, userData := range req.GetUsersData() {
		err := s.applyUserDataWithRetry(ctx, userData, s.repopulateUserApplyTimeout)
		if err != nil {
			u := userData.GetUser()
			serviceLogger().Warn("repopulate user skipped", "uid", u.GetId(), "username", u.GetUsername(), "error", err)
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
		uCtx, cancel := context.WithTimeout(ctx, s.repopulateUserApplyTimeout)
		err := s.removeUser(uCtx, user, user.Inbounds)
		cancel()
		if err != nil {
			serviceLogger().Warn("repopulate cleanup skipped", "uid", user.ID, "username", user.Username, "error", err)
			continue
		}
		s.store.RemoveUser(user.ID)
	}
	return &servicepb.Empty{}, nil
}

func (s *Service) FetchBackends(ctx context.Context, _ *servicepb.Empty) (*servicepb.BackendsResponse, error) {
	_ = ctx
	logger := serviceLogger()
	out := &servicepb.BackendsResponse{
		Backends: make([]*servicepb.Backend, 0, len(s.backends)),
	}
	for name, b := range s.backends {
		inbounds := b.ListInbounds()
		pbInbounds := make([]*servicepb.Inbound, 0, len(inbounds))
		for _, inb := range inbounds {
			raw, err := json.Marshal(inb.Config)
			if err != nil {
				logger.Warn("backend inbound config marshal failed", "backend", name, "tag", inb.Tag, "error", err)
				raw = []byte("{}")
			}
			rawStr := string(raw)
			pbInbounds = append(pbInbounds, &servicepb.Inbound{
				Tag:    inb.Tag,
				Config: &rawStr,
			})
		}
		out.Backends = append(out.Backends, &servicepb.Backend{
			Name:     name,
			Type:     strPtr(b.Type()),
			Version:  strPtr(sanitizeBackendVersion(b.Version())),
			Inbounds: pbInbounds,
		})
	}
	return out, nil
}

func (s *Service) FetchUsersStats(ctx context.Context, _ *servicepb.Empty) (*servicepb.UsersStats, error) {
	logger := serviceLogger()
	all := map[uint32]uint64{}
	for name, b := range s.backends {
		stats, err := b.GetUsages(ctx, true)
		if err != nil {
			logger.Warn("fetch users stats failed", "backend", name, "error", err)
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
	logger := serviceLogger()
	b, ok := s.backends[req.GetBackendName()]
	if !ok {
		logger.Warn("stream backend logs failed: backend not found", "backend", req.GetBackendName())
		return status.Error(codes.NotFound, "backend not found")
	}
	logs, err := b.LogStream(stream.Context(), req.GetIncludeBuffer())
	if err != nil {
		logger.Error("stream backend logs failed", "backend", req.GetBackendName(), "error", err)
		return status.Errorf(codes.Unavailable, "log stream failed: %v", err)
	}
	for line := range logs {
		if err := stream.Send(&servicepb.LogLine{Line: line}); err != nil {
			logger.Warn("stream backend logs send failed", "backend", req.GetBackendName(), "error", err)
			return err
		}
	}
	return nil
}

func (s *Service) FetchBackendConfig(ctx context.Context, req *servicepb.Backend) (*servicepb.BackendConfig, error) {
	logger := serviceLogger()
	b, ok := s.backends[req.GetName()]
	if !ok {
		logger.Warn("fetch backend config failed: backend not found", "backend", req.GetName())
		return nil, status.Error(codes.NotFound, "backend not found")
	}
	cfg, err := b.GetConfig(ctx)
	if err != nil {
		logger.Error("fetch backend config failed", "backend", req.GetName(), "error", err)
		return nil, status.Errorf(codes.Internal, "fetch config failed: %v", err)
	}
	return &servicepb.BackendConfig{
		Configuration: cfg,
		ConfigFormat:  servicepb.ConfigFormat(b.ConfigFormat()),
	}, nil
}

func (s *Service) RestartBackend(ctx context.Context, req *servicepb.RestartBackendRequest) (*servicepb.Empty, error) {
	logger := serviceLogger()
	b, ok := s.backends[req.GetBackendName()]
	if !ok {
		logger.Warn("backend restart failed: backend not found", "backend", req.GetBackendName())
		return nil, status.Error(codes.NotFound, "backend not found")
	}
	cfg := ""
	if req.GetConfig() != nil {
		cfg = req.GetConfig().GetConfiguration()
	}
	logger.Info("backend restart requested", "backend", req.GetBackendName())
	if err := b.Restart(ctx, cfg); err != nil {
		logger.Error("backend restart failed", "backend", req.GetBackendName(), "error", err)
		return nil, status.Errorf(codes.Unavailable, "backend restart failed: %v", err)
	}
	logger.Info("backend restart completed", "backend", req.GetBackendName())
	return &servicepb.Empty{}, nil
}

func (s *Service) GetBackendStats(ctx context.Context, req *servicepb.Backend) (*servicepb.BackendStats, error) {
	_ = ctx
	logger := serviceLogger()
	b, ok := s.backends[req.GetName()]
	if !ok {
		logger.Warn("get backend stats failed: backend not found", "backend", req.GetName())
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

func sanitizeBackendVersion(version string) string {
	version = strings.TrimSpace(version)
	if len(version) <= 32 {
		return version
	}
	return version[:32]
}

func isExpectedStreamClose(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	return status.Code(err) == codes.Canceled
}
