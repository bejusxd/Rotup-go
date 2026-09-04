package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/removable"
)

// fakeMedia records what the disks page asked for. It never performs any real
// media work, so the handlers can be exercised on any platform.
type fakeMedia struct {
	mu sync.Mutex

	snapshot    removable.Snapshot
	available   bool
	registerErr error
	mountErr    error
	unmountErr  error

	registered []registerCall
	updated    []updateCall
	mounted    []string
	unmounted  []string
	// contexts records the context each operation ran under, so the test can
	// prove a media operation is not tied to the HTTP request.
	contexts []context.Context
}

type registerCall struct{ UUID, Name string }
type updateCall struct {
	ID      string
	Name    string
	Enabled bool
}

func (f *fakeMedia) Snapshot(context.Context) (removable.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot, nil
}

func (f *fakeMedia) Register(_ context.Context, uuid, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.registerErr != nil {
		return "", f.registerErr
	}
	f.registered = append(f.registered, registerCall{UUID: uuid, Name: name})
	return "new-disk-id", nil
}

func (f *fakeMedia) UpdateSettings(_ context.Context, id, name string, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updated = append(f.updated, updateCall{ID: id, Name: name, Enabled: enabled})
	return nil
}

func (f *fakeMedia) Mount(ctx context.Context, diskID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contexts = append(f.contexts, ctx)
	if f.mountErr != nil {
		return f.mountErr
	}
	f.mounted = append(f.mounted, diskID)
	return nil
}

func (f *fakeMedia) SyncAndUnmount(ctx context.Context, diskID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contexts = append(f.contexts, ctx)
	if f.unmountErr != nil {
		return f.unmountErr
	}
	f.unmounted = append(f.unmounted, diskID)
	return nil
}

func (f *fakeMedia) PlatformAvailable() bool { return f.available }
func (f *fakeMedia) Busy(string) bool        { return false }

const testDiskID = "22222222-2222-4222-8222-222222222222"

func testDisk() *domain.Disk {
	return &domain.Disk{
		ID: testDiskID, Name: "backup A",
		FilesystemUUID: "11111111-2222-3333-4444-555555555555",
		FilesystemType: "ext4", Enabled: true,
		CurrentStatus: domain.DiskAvailableUnmounted,
	}
}

func testObservation(devicePath, uuid, fsType string) *domain.ObservedMedia {
	return &domain.ObservedMedia{
		DevicePath: devicePath, FilesystemUUID: uuid, FilesystemType: fsType,
		SizeBytes: 1024 * 1024 * 1024, Removable: true, Transport: "usb",
		ObservedAt: time.Now().UTC(),
	}
}

// newDiskServer builds a server whose media seam is the fake.
func newDiskServer(t *testing.T, media *fakeMedia) *testServer {
	t.Helper()
	server := newTestServer(t)

	handler, err := NewHandler(Deps{
		Version: "dev",
		Status:  server.backend,
		Repo:    server.backend,
		Admin:   server.backend,
		Runner:  server.backend,
		Staging: server.layout,
		Media:   media,
		Engine:  func() EngineStatus { return EngineStatus{Name: "rclone", Available: true} },
		Background: func() context.Context {
			return context.WithValue(context.Background(), backgroundMarker{}, true)
		},
	})
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	server.handler = handler
	server.token = server.fetchToken(t, "/legacy/sources/new")
	return server
}

type backgroundMarker struct{}

func TestDisksPageShowsRegisteredAndObservedMedia(t *testing.T) {
	t.Parallel()

	media := &fakeMedia{available: true}
	media.snapshot = removable.Snapshot{
		PlatformAvailable: true,
		Views: []removable.MediaView{
			{
				Disk: testDisk(), Status: domain.DiskAvailableUnmounted, Supported: true,
				Observed: testObservation("/dev/sdb1", "11111111-2222-3333-4444-555555555555", "ext4"),
			},
			{
				Observed: testObservation("/dev/sdc1", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "ext4"),
				Status:   domain.DiskDetected, Supported: true,
			},
		},
	}
	server := newDiskServer(t, media)

	response := server.get(t, "/legacy/disks")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	body := response.Body.String()
	for _, fragment := range []string{
		"backup A", "11111111-2222-3333-4444-555555555555", "ext4",
		"AVAILABLE_UNMOUNTED", "/dev/sdb1", "/dev/sdc1", "usb", "1.0 GiB",
		"Mount and verify", "Register",
	} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("the disks page does not contain %q", fragment)
		}
	}
	// Media management must never offer a destructive action.
	for _, forbidden := range []string{"Format", "Partition", "Initialize repository", "Backup now", "Prune"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the disks page offers %q, which is not an M3 action", forbidden)
		}
	}
}

func TestUnregisteredMediaOffersOnlyRegistration(t *testing.T) {
	t.Parallel()

	media := &fakeMedia{available: true}
	media.snapshot = removable.Snapshot{
		PlatformAvailable: true,
		Views: []removable.MediaView{
			{Observed: testObservation("/dev/sdc1", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "ext4"),
				Status: domain.DiskDetected, Supported: true},
		},
	}
	server := newDiskServer(t, media)
	body := server.get(t, "/legacy/disks").Body.String()

	if strings.Contains(body, "/mount") || strings.Contains(body, "/unmount") {
		t.Fatal("unregistered media must not be offered a mount action")
	}
	if !strings.Contains(body, "/legacy/disks/register") {
		t.Fatal("unregistered media should offer registration")
	}
}

func TestUnsupportedAndAmbiguousMediaCannotBeRegistered(t *testing.T) {
	t.Parallel()

	media := &fakeMedia{available: true}
	media.snapshot = removable.Snapshot{
		PlatformAvailable: true,
		Views: []removable.MediaView{
			{Observed: testObservation("/dev/sdc1", "aaaaaaaa-bbbb", "ntfs"),
				Status: domain.DiskDetected, Supported: false},
			{Observed: testObservation("/dev/sdd1", "cccccccc-dddd", "ext4"),
				Status: domain.DiskDetected, Supported: true, Ambiguous: true},
			{Observed: testObservation("/dev/sde1", "", "ext4"),
				Status: domain.DiskDetected, Supported: true},
		},
	}
	server := newDiskServer(t, media)
	body := server.get(t, "/legacy/disks").Body.String()

	if strings.Count(body, `action="/legacy/disks/register"`) != 0 {
		t.Fatal("media that cannot be registered must not show a registration form")
	}
	for _, fragment := range []string{"duplicate UUID", "not a filesystem ROTUP manages", "no persistent UUID"} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("the page does not explain why registration is unavailable: %q missing", fragment)
		}
	}
}

func TestRegisterRequiresAFormToken(t *testing.T) {
	t.Parallel()

	media := &fakeMedia{available: true}
	server := newDiskServer(t, media)

	response := server.post(t, "/legacy/disks/register",
		url.Values{"filesystem_uuid": {"1111"}, "name": {"backup A"}}, false)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	if len(media.registered) != 0 {
		t.Fatal("a request without a form token must not register anything")
	}
}

func TestRegisterSucceedsAndRedirects(t *testing.T) {
	t.Parallel()

	media := &fakeMedia{available: true}
	server := newDiskServer(t, media)

	response := server.post(t, "/legacy/disks/register", url.Values{
		"filesystem_uuid": {"11111111-2222-3333-4444-555555555555"},
		"name":            {"backup A"},
	}, true)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Location") != "/legacy/disks" {
		t.Fatalf("redirect = %q", response.Header().Get("Location"))
	}
	if len(media.registered) != 1 {
		t.Fatalf("registered %d disks, want 1", len(media.registered))
	}
	if media.registered[0].UUID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("registered = %#v", media.registered[0])
	}
}

func TestRegisterFailureIsShownSafely(t *testing.T) {
	t.Parallel()

	media := &fakeMedia{available: true}
	media.registerErr = domain.NewError(domain.ErrClassIntegrity, "ambiguous_identity",
		"more than one connected filesystem claims that UUID")
	server := newDiskServer(t, media)

	response := server.post(t, "/legacy/disks/register",
		url.Values{"filesystem_uuid": {"1111"}, "name": {"backup A"}}, true)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	if !strings.Contains(response.Body.String(), "more than one connected filesystem") {
		t.Fatalf("the reason is not shown: %s", response.Body.String())
	}
}

func TestMountAndUnmountAreExplicitProtectedActions(t *testing.T) {
	t.Parallel()

	media := &fakeMedia{available: true}
	server := newDiskServer(t, media)

	if code := server.post(t, "/legacy/disks/"+testDiskID+"/mount", url.Values{}, true).Code; code != http.StatusSeeOther {
		t.Fatalf("mount status = %d", code)
	}
	if len(media.mounted) != 1 || media.mounted[0] != testDiskID {
		t.Fatalf("mounted = %#v", media.mounted)
	}
	if code := server.post(t, "/legacy/disks/"+testDiskID+"/unmount", url.Values{}, true).Code; code != http.StatusSeeOther {
		t.Fatalf("unmount status = %d", code)
	}
	if len(media.unmounted) != 1 {
		t.Fatalf("unmounted = %#v", media.unmounted)
	}

	// Neither may be triggered without a form token.
	if code := server.post(t, "/legacy/disks/"+testDiskID+"/mount", url.Values{}, false).Code; code != http.StatusBadRequest {
		t.Fatalf("an unprotected mount returned %d", code)
	}
	if code := server.post(t, "/legacy/disks/"+testDiskID+"/unmount", url.Values{}, false).Code; code != http.StatusBadRequest {
		t.Fatalf("an unprotected unmount returned %d", code)
	}
	if len(media.mounted) != 1 || len(media.unmounted) != 1 {
		t.Fatal("an unprotected request must not reach the media service")
	}
}

// A browser that goes away must not be able to cancel a mount partway through.
func TestMediaOperationsRunOnTheDaemonContext(t *testing.T) {
	t.Parallel()

	media := &fakeMedia{available: true}
	server := newDiskServer(t, media)

	if code := server.post(t, "/legacy/disks/"+testDiskID+"/mount", url.Values{}, true).Code; code != http.StatusSeeOther {
		t.Fatalf("mount status = %d", code)
	}
	media.mu.Lock()
	defer media.mu.Unlock()
	if len(media.contexts) != 1 {
		t.Fatalf("recorded %d contexts, want 1", len(media.contexts))
	}
	if media.contexts[0].Value(backgroundMarker{}) != true {
		t.Fatal("a media operation must run on the daemon's context, not the request's")
	}
}

func TestMountFailureIsReportedWithoutClaimingSuccess(t *testing.T) {
	t.Parallel()

	media := &fakeMedia{available: true}
	media.mountErr = domain.NewError(domain.ErrClassIntegrity, "identity_mismatch",
		"the filesystem mounted at the expected mount point carries a different UUID")
	server := newDiskServer(t, media)

	response := server.post(t, "/legacy/disks/"+testDiskID+"/mount", url.Values{}, true)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	if !strings.Contains(response.Body.String(), "different UUID") {
		t.Fatalf("the reason is not shown: %s", response.Body.String())
	}
	if len(media.mounted) != 0 {
		t.Fatal("a failed mount must not be recorded as mounted")
	}
}

func TestUpdateDiskMetadata(t *testing.T) {
	t.Parallel()

	media := &fakeMedia{available: true}
	server := newDiskServer(t, media)

	response := server.post(t, "/legacy/disks/"+testDiskID,
		url.Values{"name": {"renamed"}}, true)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", response.Code)
	}
	if len(media.updated) != 1 {
		t.Fatalf("updated = %#v", media.updated)
	}
	if media.updated[0].Name != "renamed" || media.updated[0].Enabled {
		t.Fatalf("update = %#v, want the unchecked box to disable the disk", media.updated[0])
	}
}

func TestUnsupportedPlatformIsExplainedRatherThanHidden(t *testing.T) {
	t.Parallel()

	media := &fakeMedia{available: false}
	media.snapshot = removable.Snapshot{
		PlatformAvailable: false,
		PlatformReason:    "ROTUP manages removable media on Linux only; this platform is for development",
	}
	server := newDiskServer(t, media)

	response := server.get(t, "/legacy/disks")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; the page must still render", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, "Linux only") {
		t.Fatalf("the platform limitation is not explained: %s", body)
	}
	if !strings.Contains(body, "No disks are registered yet") {
		t.Fatal("the page should still render its sections")
	}
}

func TestEnumerationFailureIsSurfacedOnThePage(t *testing.T) {
	t.Parallel()

	media := &fakeMedia{available: true}
	media.snapshot = removable.Snapshot{
		PlatformAvailable: true,
		EnumerationError:  "listing block devices failed",
		Views: []removable.MediaView{
			{Disk: testDisk(), Status: domain.DiskDisconnected},
		},
	}
	server := newDiskServer(t, media)
	body := server.get(t, "/legacy/disks").Body.String()

	if !strings.Contains(body, "listing block devices failed") {
		t.Fatal("an enumeration failure must be shown")
	}
	if !strings.Contains(body, "backup A") {
		t.Fatal("registered disks must stay visible when enumeration fails")
	}
}

func TestDiskTextIsEscaped(t *testing.T) {
	t.Parallel()

	media := &fakeMedia{available: true}
	disk := testDisk()
	disk.Name = `<script>alert(1)</script>`
	media.snapshot = removable.Snapshot{
		PlatformAvailable: true,
		Views: []removable.MediaView{
			{Disk: disk, Status: domain.DiskError, ErrorCode: `<img src=x onerror=alert(1)>`},
		},
	}
	server := newDiskServer(t, media)
	body := server.get(t, "/legacy/disks").Body.String()

	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("the disks page rendered unescaped markup")
	}
	if strings.Contains(body, "<img src=x onerror") {
		t.Fatal("the disks page rendered unescaped markup from an error code")
	}
}

func TestDisksRoutesAreAbsentWithoutTheMediaSeam(t *testing.T) {
	t.Parallel()

	// The M1 and M2 surface must keep working when media is not wired in.
	server := newTestServer(t)
	if code := server.get(t, "/legacy/disks").Code; code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when media is not configured", code)
	}
	if code := server.get(t, "/healthz").Code; code != http.StatusOK {
		t.Fatalf("health status = %d", code)
	}
	if code := server.get(t, "/legacy/sources").Code; code != http.StatusOK {
		t.Fatalf("sources status = %d", code)
	}
}
