package grpccontrol

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nodev1 "github.com/vanpelt/sparky/tools/sparkbox/api/node/v1"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/eventjournal"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodecert"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/operationjournal"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

type fakeBackend struct {
	mu          sync.Mutex
	boxes       map[string]*host.Sandbox
	snapshots   map[string]*host.Snapshot
	createCalls atomic.Int32
}

func TestCurrentPeerCertificateRejectsExpiredLongLivedConnection(t *testing.T) {
	now := time.Now().UTC()
	peerContext := func(notBefore, notAfter time.Time) context.Context {
		return peer.NewContext(context.Background(), &peer.Peer{
			AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{{
					NotBefore: notBefore,
					NotAfter:  notAfter,
				}},
			}},
		})
	}

	if err := requireCurrentPeerCertificate(
		peerContext(now.Add(-time.Hour), now.Add(time.Hour)), now,
	); err != nil {
		t.Fatalf("current peer certificate rejected: %v", err)
	}
	for name, ctx := range map[string]context.Context{
		"expired":    peerContext(now.Add(-2*time.Hour), now.Add(-time.Hour)),
		"not active": peerContext(now.Add(time.Hour), now.Add(2*time.Hour)),
		"missing":    context.Background(),
	} {
		t.Run(name, func(t *testing.T) {
			err := requireCurrentPeerCertificate(ctx, now)
			if status.Code(err) != codes.Unauthenticated {
				t.Fatalf("certificate check = %v, want Unauthenticated", err)
			}
		})
	}
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		boxes:     map[string]*host.Sandbox{},
		snapshots: map[string]*host.Snapshot{},
	}
}

func cloneBox(box *host.Sandbox) *host.Sandbox {
	if box == nil {
		return nil
	}
	cloned := *box
	return &cloned
}

func cloneSnapshot(snapshot *host.Snapshot) *host.Snapshot {
	if snapshot == nil {
		return nil
	}
	cloned := *snapshot
	return &cloned
}

func (f *fakeBackend) Get(name string) (*host.Sandbox, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	box, ok := f.boxes[name]
	return cloneBox(box), ok
}

func (f *fakeBackend) List() []*host.Sandbox {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*host.Sandbox, 0, len(f.boxes))
	for _, box := range f.boxes {
		out = append(out, cloneBox(box))
	}
	return out
}

func (f *fakeBackend) AllSnapshots() []*host.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*host.Snapshot, 0, len(f.snapshots))
	for _, snapshot := range f.snapshots {
		out = append(out, cloneSnapshot(snapshot))
	}
	return out
}

func (f *fakeBackend) Capacity() host.NodeCapacity {
	f.mu.Lock()
	defer f.mu.Unlock()
	return host.NodeCapacity{
		Node:       "node-a",
		Arch:       "arm64",
		Release:    "test",
		TotalVCPUs: 8,
		TotalMemMB: 16 * 1024,
		Sandboxes:  len(f.boxes),
		Online:     true,
	}
}

func (f *fakeBackend) Vitals(context.Context, string) (host.Vitals, error) {
	cpu, memory := 1.5, int64(128)
	rx, tx := uint64(10), uint64(20)
	return host.Vitals{CPUSeconds: &cpu, MemUsedMB: &memory, NetRxBytes: &rx, NetTxBytes: &tx}, nil
}

func (f *fakeBackend) EnsureReady(_ context.Context, name string) (*host.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	box, ok := f.boxes[name]
	if !ok {
		return nil, &host.MissingError{Noun: "sandbox", Name: name}
	}
	box.State = vmm.StateRunning
	return cloneBox(box), nil
}

func (f *fakeBackend) Create(_ context.Context, name, owner, image string, vcpus, memory int64) (*host.Sandbox, error) {
	f.createCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.boxes[name]; exists {
		return nil, &host.NameError{Problem: host.NameTaken, Noun: "sandbox", Name: name}
	}
	box := &host.Sandbox{
		Name: name, Owner: owner, Image: image, VCPUs: vcpus, MemMB: memory,
		State: vmm.StateRunning, CreatedAt: time.Now().UTC(), LastActive: time.Now().UTC(),
		HostIP: "10.200.0.2",
	}
	f.boxes[name] = box
	return cloneBox(box), nil
}

func (f *fakeBackend) Pause(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	box, ok := f.boxes[name]
	if !ok {
		return &host.MissingError{Noun: "sandbox", Name: name}
	}
	box.State = vmm.StatePaused
	return nil
}

func (f *fakeBackend) Archive(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	box, ok := f.boxes[name]
	if !ok {
		return &host.MissingError{Noun: "sandbox", Name: name}
	}
	box.State = vmm.StateArchived
	return nil
}

func (f *fakeBackend) Resize(_ context.Context, name string, size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	box, ok := f.boxes[name]
	if !ok {
		return &host.MissingError{Noun: "sandbox", Name: name}
	}
	box.DiskTotalMB = size
	return nil
}

func (f *fakeBackend) Reboot(_ context.Context, name string) error {
	_, ok := f.Get(name)
	if !ok {
		return &host.MissingError{Noun: "sandbox", Name: name}
	}
	return nil
}

func (f *fakeBackend) Rename(_ context.Context, oldName, newName, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	box, ok := f.boxes[oldName]
	if !ok || box.Owner != owner {
		return &host.MissingError{Noun: "sandbox", Name: oldName}
	}
	delete(f.boxes, oldName)
	box.Name = newName
	f.boxes[newName] = box
	return nil
}

func (f *fakeBackend) Destroy(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.boxes[name]; !ok {
		return &host.MissingError{Noun: "sandbox", Name: name}
	}
	delete(f.boxes, name)
	return nil
}

func (f *fakeBackend) SetPinned(name string, pinned bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	box, ok := f.boxes[name]
	if !ok {
		return &host.MissingError{Noun: "sandbox", Name: name}
	}
	box.Pinned = pinned
	return nil
}

func (f *fakeBackend) ResyncEnv(context.Context, string) {}

func (f *fakeBackend) Snapshot(_ context.Context, boxName, snapshotName, owner string) (*host.Snapshot, error) {
	if _, ok := f.Get(boxName); !ok {
		return nil, &host.MissingError{Noun: "sandbox", Name: boxName}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	snapshot := &host.Snapshot{
		Name: snapshotName, Owner: owner, Image: "snap-" + owner + "-" + snapshotName,
		FromBox: boxName, CreatedAt: time.Now().UTC(), Node: "node-a",
	}
	f.snapshots[owner+"/"+snapshotName] = snapshot
	return cloneSnapshot(snapshot), nil
}

func (f *fakeBackend) DeleteSnapshot(_ context.Context, name, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := owner + "/" + name
	if _, ok := f.snapshots[key]; !ok {
		return &host.MissingError{Noun: "snapshot", Name: name}
	}
	delete(f.snapshots, key)
	return nil
}

func (f *fakeBackend) Fork(ctx context.Context, snapshot, name, owner string, vcpus, memory int64) (*host.Sandbox, error) {
	f.mu.Lock()
	_, ok := f.snapshots[owner+"/"+snapshot]
	f.mu.Unlock()
	if !ok {
		return nil, &host.MissingError{Noun: "snapshot", Name: snapshot}
	}
	return f.Create(ctx, name, owner, "snapshot", vcpus, memory)
}

func (f *fakeBackend) MarkActive(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if box := f.boxes[name]; box != nil {
		box.LastActive = time.Now().UTC()
	}
}

func (f *fakeBackend) RecordKey(name, fingerprint string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if box := f.boxes[name]; box != nil {
		box.KeyFP = fingerprint
	}
}

type testPKI struct {
	authority *nodecert.CA
	roots     *x509.CertPool
	node      tls.Certificate
	gateway   tls.Certificate
}

func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	authority, _, _, err := nodecert.NewCA("test-cluster")
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(authority.Certificate())
	return testPKI{
		authority: authority,
		roots:     roots,
		node:      issueCertificate(t, authority, nodecert.Peer{Role: nodecert.RoleNode, Name: "node-a"}),
		gateway:   issueCertificate(t, authority, nodecert.Peer{Role: nodecert.RoleGateway, Name: "cluster-a"}),
	}
}

func issueCertificate(t *testing.T, authority *nodecert.CA, peer nodecert.Peer) tls.Certificate {
	t.Helper()
	key, csr, err := nodecert.NewCSR(peer.Name)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM, _, _, err := authority.SignCSR(csr, peer, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

type testRig struct {
	client     *Client
	raw        nodev1.NodeControlClient
	backend    *fakeBackend
	operations *operationjournal.Journal
	events     *eventjournal.Journal
	listener   *bufconn.Listener
	server     *grpc.Server
	cancel     context.CancelFunc
}

func newTestRig(t *testing.T, retain int, interceptor grpc.UnaryClientInterceptor) *testRig {
	t.Helper()
	backend := newFakeBackend()
	operations, err := operationjournal.Open(t.TempDir() + "/operations.db")
	if err != nil {
		t.Fatal(err)
	}
	events, err := eventjournal.Open(t.TempDir()+"/events.db", retain)
	if err != nil {
		operations.Close()
		t.Fatal(err)
	}
	runContext, cancel := context.WithCancel(context.Background())
	service, err := NewServer(ServerConfig{
		Context: runContext, Backend: backend, Operations: operations, Events: events,
		Node: "node-a", Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	pki := newTestPKI(t)
	serverTLS, err := nodecert.ServerTLSConfig(
		pki.node, pki.roots,
		nodecert.Peer{Role: nodecert.RoleGateway, Name: "cluster-a"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewRPCServer(service, serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	go func() {
		_ = server.Serve(listener)
	}()
	clientTLS, err := nodecert.ClientTLSConfig(
		pki.gateway, pki.roots,
		nodecert.Peer{Role: nodecert.RoleNode, Name: "node-a"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	options := []grpc.DialOption{
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithBlock(),
	}
	if interceptor != nil {
		options = append(options, grpc.WithUnaryInterceptor(interceptor))
	}
	dialContext, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	client, err := DialTLS(dialContext, "bufnet", clientTLS, options...)
	if err != nil {
		t.Fatal(err)
	}
	rig := &testRig{
		client: client, raw: client.Raw(), backend: backend,
		operations: operations, events: events, listener: listener, server: server, cancel: cancel,
	}
	t.Cleanup(func() {
		rig.client.Close()
		rig.cancel()
		rig.server.Stop()
		rig.listener.Close()
		rig.operations.Close()
		rig.events.Close()
	})
	return rig
}

func createRequest(t *testing.T, name, operationID, key string) *nodev1.CreateRequest {
	t.Helper()
	request := &nodev1.CreateRequest{
		Name: name, Owner: "alice", Image: "ubuntu", Vcpus: 2, MemoryMb: 1024,
	}
	identity, err := NewOperationIdentity(request, operationID, key, "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	request.Operation = identity
	return request
}

func TestLostBeginReplyReattachesWithoutDuplicateMutation(t *testing.T) {
	var loseReply atomic.Bool
	loseReply.Store(true)
	interceptor := func(
		ctx context.Context,
		method string,
		request, reply any,
		connection *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		options ...grpc.CallOption,
	) error {
		err := invoker(ctx, method, request, reply, connection, options...)
		if err == nil && method == nodev1.NodeControl_BeginCreate_FullMethodName && loseReply.CompareAndSwap(true, false) {
			return status.Error(codes.Unavailable, "reply lost after handler committed")
		}
		return err
	}
	rig := newTestRig(t, 16, interceptor)
	request := createRequest(t, "demo", "operation-create", "idempotency-create")

	operation, err := rig.client.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if operation.GetState() != nodev1.OperationState_OPERATION_STATE_SUCCEEDED {
		t.Fatalf("operation state = %s", operation.GetState())
	}
	if got := rig.backend.createCalls.Load(); got != 1 {
		t.Fatalf("create calls = %d, want 1", got)
	}

	if _, err := rig.client.Create(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if got := rig.backend.createCalls.Load(); got != 1 {
		t.Fatalf("create calls after duplicate = %d, want 1", got)
	}
}

func TestDuplicateIdentityWithDifferentHashConflicts(t *testing.T) {
	rig := newTestRig(t, 16, nil)
	first := createRequest(t, "alpha", "operation-conflict", "idempotency-conflict")
	if _, err := rig.client.Create(context.Background(), first); err != nil {
		t.Fatal(err)
	}

	conflicting := createRequest(t, "beta", "operation-conflict", "idempotency-conflict")
	_, err := rig.raw.BeginCreate(context.Background(), conflicting)
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("BeginCreate conflict error = %v, want AlreadyExists", err)
	}
	if got := rig.backend.createCalls.Load(); got != 1 {
		t.Fatalf("create calls = %d, want 1", got)
	}
}

func TestEventReplayGapProducesReconciliationSignal(t *testing.T) {
	rig := newTestRig(t, 2, nil)
	for i := 0; i < 3; i++ {
		payload, err := proto.Marshal(sandboxGone("old"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rig.events.Append(context.Background(), eventSandboxGone, payload); err != nil {
			t.Fatal(err)
		}
	}

	events, errs := rig.client.WatchEvents(context.Background(), 0)
	event, ok := <-events
	if !ok {
		t.Fatal("event stream closed without gap")
	}
	gap := event.GetGap()
	if gap == nil {
		t.Fatalf("event = %v, want gap", event)
	}
	if gap.GetOldestAvailableRevision() != 2 || gap.GetCurrentRevision() != 3 || event.GetRevision() != 3 {
		t.Fatalf("gap = %+v at revision %d, want oldest=2 current=3", gap, event.GetRevision())
	}
	if err, ok := <-errs; ok && err != nil {
		t.Fatalf("watch error = %v", err)
	}
}

func TestNewServerRecoversInterruptedOperationWithoutRepeatingMutation(t *testing.T) {
	dir := t.TempDir()
	operationPath := dir + "/operations.db"
	operations, err := operationjournal.Open(operationPath)
	if err != nil {
		t.Fatal(err)
	}
	request := createRequest(t, "demo", "operation-interrupted", "idempotency-interrupted")
	spec, err := operationSpec(request.GetOperation(), "create", request.GetName(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := operations.Claim(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if _, err := operations.Start(context.Background(), spec.ID); err != nil {
		t.Fatal(err)
	}
	if err := operations.Close(); err != nil {
		t.Fatal(err)
	}
	operations, err = operationjournal.Open(operationPath)
	if err != nil {
		t.Fatal(err)
	}
	defer operations.Close()
	events, err := eventjournal.Open(dir+"/events.db", 8)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	backend := newFakeBackend()
	service, err := NewServer(ServerConfig{
		Backend: backend, Operations: operations, Events: events,
	})
	if err != nil {
		t.Fatal(err)
	}

	replayed, err := service.BeginCreate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.GetState() != nodev1.OperationState_OPERATION_STATE_FAILED ||
		replayed.GetError().GetCode() != interruptedOperationCode ||
		!replayed.GetError().GetRetryable() {
		t.Fatalf("recovered operation = %+v", replayed)
	}
	if got := backend.createCalls.Load(); got != 0 {
		t.Fatalf("backend create calls = %d, want no replay after restart", got)
	}
}

func TestPostSideEffectEventFailureIsTerminalAndDoesNotReplay(t *testing.T) {
	dir := t.TempDir()
	operations, err := operationjournal.Open(dir + "/operations.db")
	if err != nil {
		t.Fatal(err)
	}
	defer operations.Close()
	events, err := eventjournal.Open(dir+"/events.db", 8)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewServer(ServerConfig{
		Backend: newFakeBackend(), Operations: operations, Events: events,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := createRequest(t, "demo", "operation-event-fault", "idempotency-event-fault")
	spec, err := operationSpec(request.GetOperation(), "create", request.GetName(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := operations.Claim(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if err := events.Close(); err != nil {
		t.Fatal(err)
	}

	var sideEffects atomic.Int32
	action := func(context.Context) (mutationResult, error) {
		sideEffects.Add(1)
		return mutationResult{
			result: emptyResult(),
			events: []*nodev1.InventoryEvent{snapshotGone("snapshot-a")},
		}, nil
	}
	service.execute(spec.ID, action)
	op, err := operations.Get(context.Background(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != operationjournal.StateFailed || op.Failure == nil ||
		op.Failure.Code != interruptedOperationCode || !op.Failure.Retryable {
		t.Fatalf("operation after event-journal fault = %+v", op)
	}

	replayed, err := service.begin(
		context.Background(), request.GetOperation(), "create", request.GetName(), request, action,
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.GetState() != nodev1.OperationState_OPERATION_STATE_FAILED {
		t.Fatalf("replayed operation = %+v", replayed)
	}
	if got := sideEffects.Load(); got != 1 {
		t.Fatalf("side effects = %d, want exactly one", got)
	}
}

func TestPostSideEffectOperationJournalFailureRecoversAfterReopen(t *testing.T) {
	dir := t.TempDir()
	operationPath := dir + "/operations.db"
	operations, err := operationjournal.Open(operationPath)
	if err != nil {
		t.Fatal(err)
	}
	events, err := eventjournal.Open(dir+"/events.db", 8)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	service, err := NewServer(ServerConfig{
		Backend: newFakeBackend(), Operations: operations, Events: events,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := createRequest(t, "demo", "operation-result-fault", "idempotency-result-fault")
	started := make(chan struct{})
	release := make(chan struct{})
	var sideEffects atomic.Int32
	action := func(context.Context) (mutationResult, error) {
		close(started)
		<-release
		sideEffects.Add(1)
		return mutationResult{result: emptyResult()}, nil
	}
	if _, err := service.begin(
		context.Background(), request.GetOperation(), "create", request.GetName(), request, action,
	); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("mutation did not start")
	}
	if err := operations.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err = service.Shutdown(shutdownCtx)
	shutdownCancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown with unavailable operation journal error = %v, want deadline", err)
	}
	drainCtx, drainCancel := context.WithTimeout(context.Background(), time.Second)
	if err := service.Shutdown(drainCtx); err != nil {
		drainCancel()
		t.Fatalf("drain mutation after stopping persistence retries: %v", err)
	}
	drainCancel()

	operations, err = operationjournal.Open(operationPath)
	if err != nil {
		t.Fatal(err)
	}
	defer operations.Close()
	restarted, err := NewServer(ServerConfig{
		Backend: newFakeBackend(), Operations: operations, Events: events,
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.begin(
		context.Background(), request.GetOperation(), "create", request.GetName(), request, action,
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.GetState() != nodev1.OperationState_OPERATION_STATE_FAILED ||
		replayed.GetError().GetCode() != interruptedOperationCode {
		t.Fatalf("recovered operation = %+v", replayed)
	}
	if got := sideEffects.Load(); got != 1 {
		t.Fatalf("side effects = %d, want exactly one after journal reopen", got)
	}
}

func TestMTLSRejectsUnexpectedGatewayIdentity(t *testing.T) {
	backend := newFakeBackend()
	operations, err := operationjournal.Open(t.TempDir() + "/operations.db")
	if err != nil {
		t.Fatal(err)
	}
	defer operations.Close()
	events, err := eventjournal.Open(t.TempDir()+"/events.db", 8)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	service, err := NewServer(ServerConfig{Backend: backend, Operations: operations, Events: events, Node: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	pki := newTestPKI(t)
	serverTLS, err := nodecert.ServerTLSConfig(
		pki.node, pki.roots,
		nodecert.Peer{Role: nodecert.RoleGateway, Name: "cluster-a"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewRPCServer(service, serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	listener := bufconn.Listen(1024 * 1024)
	defer listener.Close()
	go func() { _ = server.Serve(listener) }()

	wrongGateway := issueCertificate(t, pki.authority, nodecert.Peer{Role: nodecert.RoleGateway, Name: "other-cluster"})
	clientTLS, err := nodecert.ClientTLSConfig(
		wrongGateway, pki.roots,
		nodecert.Peer{Role: nodecert.RoleNode, Name: "node-a"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	client, err := DialTLS(ctx, "bufnet", clientTLS,
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithBlock(),
	)
	if err == nil {
		defer client.Close()
		_, err = client.Health(ctx)
	}
	if err == nil {
		t.Fatal("unexpected gateway identity was accepted")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) && status.Code(err) == codes.OK {
		t.Fatalf("identity rejection error = %v", err)
	}
}

func TestTLSConstructorsRefuseNilConfiguration(t *testing.T) {
	operations, err := operationjournal.Open(t.TempDir() + "/operations.db")
	if err != nil {
		t.Fatal(err)
	}
	defer operations.Close()
	events, err := eventjournal.Open(t.TempDir()+"/events.db", 8)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	service, err := NewServer(ServerConfig{Backend: newFakeBackend(), Operations: operations, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewRPCServer(service, nil); err == nil {
		t.Fatal("NewRPCServer accepted nil TLS")
	}
	if _, err := DialTLS(context.Background(), "node", nil); err == nil {
		t.Fatal("DialTLS accepted nil TLS")
	}
	pki := newTestPKI(t)
	if _, err := NewRPCServer(service, &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{pki.node},
	}); err == nil {
		t.Fatal("NewRPCServer accepted TLS without required client authentication")
	}
	if _, err := NewRPCServer(service, &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{pki.node},
		ClientAuth:   tls.RequireAnyClientCert,
	}); err == nil {
		t.Fatal("NewRPCServer accepted mTLS without gateway SPIFFE verification")
	}
	if _, err := NewRPCServer(service, &tls.Config{
		MinVersion:       tls.VersionTLS12,
		Certificates:     []tls.Certificate{pki.node},
		ClientAuth:       tls.RequireAnyClientCert,
		VerifyConnection: func(tls.ConnectionState) error { return nil },
	}); err == nil {
		t.Fatal("NewRPCServer accepted TLS below version 1.3")
	}
	if _, err := DialTLS(context.Background(), "node", &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    pki.roots,
	}); err == nil {
		t.Fatal("DialTLS accepted TLS without a gateway client certificate")
	}
	if _, err := DialTLS(context.Background(), "node", &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{pki.gateway},
		RootCAs:      pki.roots,
	}); err == nil {
		t.Fatal("DialTLS accepted mTLS without node SPIFFE verification")
	}
	if _, err := DialTLS(context.Background(), "node", &tls.Config{
		MinVersion:       tls.VersionTLS12,
		Certificates:     []tls.Certificate{pki.gateway},
		RootCAs:          pki.roots,
		VerifyConnection: func(tls.ConnectionState) error { return nil },
	}); err == nil {
		t.Fatal("DialTLS accepted TLS below version 1.3")
	}
}
