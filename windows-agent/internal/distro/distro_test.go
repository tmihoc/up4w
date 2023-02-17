package distro_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canonical/ubuntu-pro-for-windows/windows-agent/internal/distro"
	"github.com/canonical/ubuntu-pro-for-windows/wslserviceapi"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"github.com/ubuntu/gowsl"
	"golang.org/x/sys/windows"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestMain(m *testing.M) {
	log.SetLevel(log.DebugLevel)

	exit := m.Run()
	defer os.Exit(exit)
}

func TestNew(t *testing.T) {
	registeredDistro, registeredGUID := registerDistro(t, false)
	_, anotherRegisteredGUID := registerDistro(t, false)

	nonRegisteredDistro := generateDistroName(t)
	fakeGUID, err := windows.GUIDFromString(`{12345678-1234-1234-1234-123456789ABC}`)
	require.NoError(t, err, "Setup: could not construct fake GUID")

	props := distro.Properties{
		DistroID:    "ubuntu",
		VersionID:   "100.04",
		PrettyName:  "Ubuntu 100.04.0 LTS",
		ProAttached: true,
	}

	testCases := map[string]struct {
		distro   string
		withGUID windows.GUID

		wantErrType error
	}{
		"Registered distro":               {distro: registeredDistro},
		"Registered distro with its GUID": {distro: registeredDistro, withGUID: registeredGUID},

		// Error cases
		"Registered distro, another distro's GUID":          {distro: nonRegisteredDistro, withGUID: anotherRegisteredGUID, wantErrType: &distro.NotExistError{}},
		"Registered distro, non-matching GUID":              {distro: registeredDistro, withGUID: fakeGUID, wantErrType: &distro.NotExistError{}},
		"Non-registered distro":                             {distro: nonRegisteredDistro, wantErrType: &distro.NotExistError{}},
		"Non-registered distro, another distro's GUID":      {distro: nonRegisteredDistro, withGUID: registeredGUID, wantErrType: &distro.NotExistError{}},
		"Non-registered distro, with a non-registered GUID": {distro: nonRegisteredDistro, withGUID: fakeGUID, wantErrType: &distro.NotExistError{}},
	}

	for name, tc := range testCases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			var d *distro.Distro
			var err error

			var args []distro.Option
			nilGUID := windows.GUID{}
			if tc.withGUID != nilGUID {
				args = append(args, distro.WithGUID(tc.withGUID))
			}

			d, err = distro.New(tc.distro, props, args...)
			if err == nil {
				defer d.Cleanup(context.Background())
			}
			if tc.wantErrType != nil {
				require.Error(t, err, "New() should have returned an error")
				require.ErrorIsf(t, err, tc.wantErrType, "New() should have returned an error of type %T", tc.wantErrType)
				return
			}

			require.NoError(t, err, "New() should have returned no error")
			require.Equal(t, tc.distro, d.Name, "distro.Name should match the one it was constructed with")
			require.Equal(t, registeredGUID.String(), d.GUID.String(), "distro.GUID should match the one it was constructed with")
			require.Equal(t, props, d.Properties, "distro.Properties should match the one it was constructed with because they were never directly modified")
		})
	}
}

func TestString(t *testing.T) {
	name, guid := registerDistro(t, false)
	d, err := distro.New(name, distro.Properties{}, distro.WithGUID(guid))
	require.NoError(t, err, "Setup: unexpected error in distro.New")

	s := d.String()
	require.Contains(t, s, name, "String() should contain the name of the distro")
	require.Contains(t, s, strings.ToLower(guid.String()), "String() should contain the GUID of the distro")
}

func TestIsValid(t *testing.T) {
	distro1, guid1 := registerDistro(t, false)
	_, guid2 := registerDistro(t, false)

	nonRegisteredDistro := generateDistroName(t)
	fakeGUID, err := windows.GUIDFromString(`{12345678-1234-1234-1234-123456789ABC}`)
	require.NoError(t, err, "Setup: could not construct fake GUID")

	testCases := map[string]struct {
		distro string
		guid   windows.GUID

		want bool
	}{
		"registered distro with matching GUID": {distro: distro1, guid: guid1, want: true},

		// Invalid cases
		"registered distro with different, another distro's GUID": {distro: distro1, guid: guid2, want: false},
		"registered distro with different, fake GUID":             {distro: distro1, guid: fakeGUID, want: false},
		"non-registered distro, registered distro's GUID":         {distro: nonRegisteredDistro, guid: guid1, want: false},
		"non-registered distro, non-registered distro's GUID":     {distro: nonRegisteredDistro, guid: fakeGUID, want: false},
	}

	for name, tc := range testCases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			// Create an always valid distro
			d, err := distro.New(distro1, distro.Properties{})
			require.NoError(t, err, "Setup: distro New() should return no errors")

			// Change values and assert on IsValid
			d.Name = tc.distro
			d.GUID = tc.guid

			got, err := d.IsValid()
			require.NoError(t, err, "IsValid should never return an error")

			require.Equal(t, tc.want, got, "IsValid should return expected value")
		})
	}
}

func TestTaskProcessing(t *testing.T) {
	reusableDistro, _ := registerDistro(t, true)

	testCases := map[string]struct {
		unregisterAfterConstructor bool // Triggers error in trying to get distro in keepAwake
		taskError                  bool // Causes the task to always return an error
		forceConnectionTimeout     bool // Cancels the context while waiting for the GRPC connection to be established
		cancelTaskInProgress       bool // Cancels as the task is running

		wantExecuteCalls int32
	}{
		"Task is executed successfully": {wantExecuteCalls: 1},
		"Unregistered distro":           {unregisterAfterConstructor: true, wantExecuteCalls: 0},
		"Connection timeout":            {forceConnectionTimeout: true, wantExecuteCalls: 0},
		"Cancel task in progress":       {cancelTaskInProgress: true, wantExecuteCalls: 1},
		"Erroneous task":                {taskError: true, wantExecuteCalls: testTaskMaxRetries},
	}

	for name, tc := range testCases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			// Optimization: we re-use the same distro for cases when
			// the distro is not modified
			distroName := reusableDistro
			if tc.unregisterAfterConstructor {
				// Otherwise, we use a new distro
				distroName, _ = registerDistro(t, true)
			}

			d, err := distro.New(distroName, distro.Properties{}, distro.WithTaskProcessingContext(ctx))
			require.NoError(t, err, "Setup: distro New() should return no error")
			defer d.Cleanup(ctx)

			wslInstanceService := newTestService(t)
			conn := wslInstanceService.newClientConnection(t)

			// End of setup
			require.Equal(t, nil, d.Client(), "Client() should return nil when there is no connection")

			if tc.unregisterAfterConstructor {
				unregisterDistro(t, distroName)
			}

			// Submit a task, wait for distro to wake up, and wait for slightly
			// more than the client waiting tickrate
			const distroWakeUpTime = 5 * time.Second
			const clientTickPeriod = 1200 * time.Millisecond

			task := &testTask{}
			if tc.taskError {
				task.Returns = errors.New("testTask error")
			}
			if tc.cancelTaskInProgress {
				// This particular task will always retry in a loop
				// Long delay to ensure we can reliably cancell it in progress
				task.Delay = 10 * time.Second
				task.Returns = errors.New("testTask error: this error should never be triggered")
			}

			err = d.SubmitTask(task)
			require.NoError(t, err, "SubmitTask() should work without returning any errors")

			// Ensuring the distro is awakened (if registered) after task submission
			wantState := "Running"
			if tc.unregisterAfterConstructor {
				wantState = "Unregistered"
			}
			require.Eventuallyf(t, func() bool { return distroState(t, distroName) == wantState }, distroWakeUpTime, 200*time.Millisecond,
				"distro should have been %q after SubmitTask(). Current state is %q", wantState, distroState(t, distroName))

			// Testing task before an active connection is established
			// We sleep to ensure at least one tick has gone by in the "wait for connection"
			time.Sleep(clientTickPeriod)
			require.Equal(t, nil, d.Client(), "Client should return nil when there is no connection")
			require.Equal(t, int32(0), task.ExecuteCalls.Load(), "Task unexpectedly executed without a connection")

			if tc.forceConnectionTimeout {
				cancel() // Simulates a timeout
				time.Sleep(clientTickPeriod)
			}

			// Testing task with with active connection
			d.SetConnection(conn)

			if tc.wantExecuteCalls == 0 {
				time.Sleep(2 * clientTickPeriod)
				require.Equal(t, int32(0), task.ExecuteCalls.Load(), "Task executed unexpectedly")
				return
			}

			require.Eventuallyf(t, func() bool { return d.Client() != nil }, clientTickPeriod, 100*time.Millisecond,
				"Client should become non-nil after setting the connection")

			// Wait for task to start
			require.Eventuallyf(t, func() bool { return task.ExecuteCalls.Load() == tc.wantExecuteCalls }, 2*clientTickPeriod, 100*time.Millisecond,
				"Task was executed fewer times than expected. Expected %d and executed %d.", tc.wantExecuteCalls, task.ExecuteCalls.Load())

			if tc.cancelTaskInProgress {
				// Cancelling and waiting for cancellation to propagate, then ensure it did so.
				cancel()
				require.Eventually(t, func() bool { return task.WasCancelled }, 100*time.Millisecond, time.Millisecond,
					"Task should be cancelled when the task processing context is cancelled")

				// Giving some time to ensure retry is never attempted.
				time.Sleep(100 * time.Millisecond)
				require.Equal(t, tc.wantExecuteCalls, task.ExecuteCalls.Load(), "Task should not be retried after being cancelled")
				return
			}

			time.Sleep(time.Second)
			require.Equal(t, tc.wantExecuteCalls, task.ExecuteCalls.Load(), "Task executed too many times after establishing a connection")

			// Testing task without with a cleaned up distro
			d.Cleanup(ctx)

			err = d.SubmitTask(&testTask{})
			require.Error(t, err, "SubmitTask() should fail after a distro has been cleaned up")
		})
	}
}

func TestSubmitTaskFailsWithFullQueue(t *testing.T) {
	distroName, _ := registerDistro(t, false)

	d, err := distro.New(distroName, distro.Properties{})
	require.NoError(t, err, "Setup: unexpected error creating the distro")
	defer d.Cleanup(context.Background())

	// We submit a first task that will be dequeued and block task processing until
	// there is a connection (i.e. forever) or until it times out after a minute.
	err = d.SubmitTask(&testTask{})
	require.NoErrorf(t, err, "SubmitTask() should not fail when the distro is active and the queue is empty.\nSubmitted: %d.\nMax: %d", 1, distro.TaskQueueSize)

	// We fill up the queue
	var i int
	for ; i < distro.TaskQueueSize; i++ {
		err := d.SubmitTask(&testTask{})
		require.NoErrorf(t, err, "SubmitTask() should not fail when the distro is active and the queue is not full.\nSubmitted: %d.\nMax: %d", i+1, distro.TaskQueueSize)
	}

	// We ensure that adding one more task will return an error
	err = d.SubmitTask(&testTask{})
	require.Errorf(t, err, "SubmitTask() should fail when the queue is full\nSubmitted: %d.\nMax: %d", i+2, distro.TaskQueueSize)
}

func TestSetConnection(t *testing.T) {
	ctx := context.Background()
	distroName, _ := registerDistro(t, false)

	d, err := distro.New(distroName, distro.Properties{})
	require.NoError(t, err, "Setup: unexpected error creating the distro")
	defer d.Cleanup(context.Background())

	wslInstanceService1 := newTestService(t)
	conn1 := wslInstanceService1.newClientConnection(t)

	wslInstanceService2 := newTestService(t)
	conn2 := wslInstanceService2.newClientConnection(t)

	require.Equal(t, nil, d.Client(), "Client() should return nil because the connection has not been set yet")
	require.False(t, d.IsActive(), "IsActive() should return false because the connection has not been set yet")

	// Set first connection as active
	d.SetConnection(conn1)

	require.True(t, d.IsActive(), "IsActive() should return true because the connection has been set")

	// GetClient twice and ensure we ping the same service
	const service1pings = 2
	for i := 0; i < service1pings; i++ {
		c := d.Client()
		require.NotEqual(t, nil, c, "client should be non-nil after setting a connection")
		_, err = c.Ping(ctx, &wslserviceapi.Empty{})
		require.NoError(t, err, "Ping attempt #%d should have been done successfully", i)
		require.Equal(t, i+1, wslInstanceService1.pingCount, "second server should be pinged after c.Ping (iteration #%d)", i)
	}

	require.Equal(t, 0, wslInstanceService2.pingCount, "second service should not be called yet")

	// Set second connection as active
	d.SetConnection(conn2)
	require.True(t, d.IsActive(), "IsActive() should return true even if the connection has changed")

	// Ping on renewed connection (new wsl instance service) and ensure only the second service receives the pings
	c := d.Client()
	require.NotEqual(t, nil, c, "client should be non-nil after setting a connection")
	_, err = c.Ping(ctx, &wslserviceapi.Empty{})
	require.NoError(t, err, "Ping should have been done successfully")
	require.Equal(t, 1, wslInstanceService2.pingCount, "second server should be pinged after c.Ping")

	require.Equal(t, service1pings, wslInstanceService1.pingCount, "first service should not have received pings after setting the connection to the second service")

	// Set connection to nil and ensure that no pings are made
	d.SetConnection(nil)
	require.Equal(t, nil, d.Client(), "Client() should return a nil because the connection has been set to nil")
	require.False(t, d.IsActive(), "IsActive() should return false because the connection has been set to nil")

	require.Equal(t, service1pings, wslInstanceService1.pingCount, "first service should not have received pings after setting the connection to nil")
	require.Equal(t, 1, wslInstanceService2.pingCount, "second service should not have received pings after setting the connection to nil")
}

func TestSetConnectionOnClosedConnection(t *testing.T) {
	ctx := context.Background()
	distroName, _ := registerDistro(t, false)

	d, err := distro.New(distroName, distro.Properties{})
	require.NoError(t, err, "Setup: unexpected error creating the distro")
	defer d.Cleanup(context.Background())

	wslInstanceService1 := newTestService(t)
	conn1 := wslInstanceService1.newClientConnection(t)

	wslInstanceService2 := newTestService(t)
	conn2 := wslInstanceService2.newClientConnection(t)

	d.SetConnection(conn1)
	_ = conn1.Close()

	d.SetConnection(conn2)

	// New connection is functional.
	_, err = d.Client().Ping(ctx, &wslserviceapi.Empty{})
	require.NoError(t, err, "Ping should have been done successfully")
	require.Equal(t, 1, wslInstanceService2.pingCount, "second service should be called once")
}

type testService struct {
	wslserviceapi.UnimplementedWSLServer
	pingCount int
	port      uint16
}

func (s *testService) Ping(context.Context, *wslserviceapi.Empty) (*wslserviceapi.Empty, error) {
	s.pingCount++
	return &wslserviceapi.Empty{}, nil
}

// newTestService creates a testService and starts serving asyncronously.
func newTestService(t *testing.T) *testService {
	t.Helper()

	server := grpc.NewServer()

	lis, err := net.Listen("tcp4", "localhost:")
	require.NoErrorf(t, err, "Setup: could not listen.")

	fields := strings.Split(lis.Addr().String(), ":")
	portTmp, err := strconv.ParseUint(fields[len(fields)-1], 10, 16)
	require.NoError(t, err, "Setup: could not parse address")

	service := testService{port: uint16(portTmp)}
	wslserviceapi.RegisterWSLServer(server, &service)
	go func() {
		err := server.Serve(lis)
		if err != nil {
			t.Logf("Setup: server.Serve returned non-nil error: %v", err)
		}
	}()

	t.Cleanup(server.Stop)

	t.Logf("Setup: Started listening at %q", lis.Addr())

	return &service
}

func (s testService) newClientConnection(t *testing.T) *grpc.ClientConn {
	t.Helper()

	addr := fmt.Sprintf("localhost:%d", s.port)

	ctxTimeout, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctxTimeout, addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock())
	require.NoError(t, err, "Setup: could not contact the grpc server at %q", addr)

	t.Cleanup(func() { conn.Close() })

	return conn
}

const testTaskMaxRetries = 5

type testTask struct {
	// ExecuteCalls counts the number of times Execute is called
	ExecuteCalls atomic.Int32

	// Delay simulates a processing time for the task
	Delay time.Duration

	// Returns is the value that Execute will return
	Returns error

	// WasCancelled is true if the task Execute context is Done
	WasCancelled bool
}

func (t *testTask) Execute(ctx context.Context, _ wslserviceapi.WSLClient) error {
	t.ExecuteCalls.Add(1)
	select {
	case <-time.After(t.Delay):
		return t.Returns
	case <-ctx.Done():
		t.WasCancelled = true
		return ctx.Err()
	}
}

func (t *testTask) String() string {
	return "Test task"
}

func (t *testTask) ShouldRetry() bool {
	return t.ExecuteCalls.Load() < testTaskMaxRetries
}

// generateDistroName generates a distroName that is not registered.
func generateDistroName(t *testing.T) (name string) {
	t.Helper()

	for i := 0; i < 10; i++ {
		//nolint: gosec // No need to be cryptographically secure in a distro name generator
		name := fmt.Sprintf("testDistro_UP4W_%d", rand.Uint32())
		d := gowsl.NewDistro(name)
		collision, err := d.IsRegistered()
		require.NoError(t, err, "Setup: could not asssert if distro already exists")
		if collision {
			t.Logf("Name %s is already taken. Retrying", name)
			continue
		}
		return name
	}
	require.Fail(t, "could not generate unique distro name")
	return ""
}

func registerDistro(t *testing.T, realDistro bool) (distroName string, GUID windows.GUID) {
	t.Helper()
	tmpDir := t.TempDir()

	var rootFsPath string
	if !realDistro {
		rootFsPath = tmpDir + "/install.tar.gz"
		err := os.WriteFile(rootFsPath, []byte{}, 0600)
		require.NoError(t, err, "could not write empty file")
	} else {
		const appx = "UbuntuPreview"
		rootFsPath = poweshellOutputf(t, `(Get-AppxPackage | Where-Object Name -like 'CanonicalGroupLimited.%s').InstallLocation`, appx)
		require.NotEmpty(t, rootFsPath, "could not find rootfs tarball. Is %s installed?", appx)
		rootFsPath = filepath.Join(rootFsPath, "install.tar.gz")
	}

	_, err := os.Lstat(rootFsPath)
	require.NoError(t, err, "Setup: Could not stat rootFs:\n%s", rootFsPath)

	distroName = generateDistroName(t)

	// Register distro with a two minute timeout
	tk := time.AfterFunc(2*time.Minute, func() { poweshellOutputf(t, `$env:WSL_UTF8=1 ; wsl --shutdown`) })
	defer tk.Stop()
	poweshellOutputf(t, "$env:WSL_UTF8=1 ; wsl.exe --import %q %q %q", distroName, tmpDir, rootFsPath)
	tk.Stop()

	t.Cleanup(func() {
		unregisterDistro(t, distroName)
	})

	d := gowsl.NewDistro(distroName)
	GUID, err = d.GUID()
	require.NoError(t, err, "Setup: could not get distro GUID")

	return distroName, GUID
}

func unregisterDistro(t *testing.T, distroName string) {
	t.Helper()

	// Unregister distro with a two minute timeout
	tk := time.AfterFunc(2*time.Minute, func() { poweshellOutputf(t, `$env:WSL_UTF8=1 ; wsl --shutdown`) })
	defer tk.Stop()
	d := gowsl.NewDistro(distroName)
	_ = d.Unregister()
}

// poweshellOutputf runs the command (with any printf-style directives and args). It fails if the
// return value of the command is non-zero. Otherwise, it returns its combined stdout and stderr.
func poweshellOutputf(t *testing.T, command string, args ...any) string {
	t.Helper()

	cmd := fmt.Sprintf(command, args...)

	//nolint: gosec // This function is only used in tests so no arbitrary code execution here
	out, err := exec.Command("powershell", "-Command", cmd).CombinedOutput()
	require.NoError(t, err, "Non-zero return code for command:\n%s\nOutput:%s", cmd, out)

	// Convert to string and get rid of trailing endline
	return strings.TrimSuffix(string(out), "\r\n")
}

// distroState returns the state of the distro as specified by wsl.exe. Possible states:
// - Installing
// - Running
// - Stopped
// - Unregistered.
func distroState(t *testing.T, distroName string) string {
	t.Helper()

	cmd := "$env:WSL_UTF8=1 ; wsl --list --all --verbose"
	out := poweshellOutputf(t, cmd)

	rows := strings.Split(out, "\n")[1:] // [1:] to skip header
	for _, row := range rows[1:] {
		fields := strings.Fields(row)
		if fields[0] == "*" {
			fields = fields[1:]
		}
		t.Logf("Searching: %q Found:%q", distroName, fields)
		require.Len(t, fields, 3, "Output of %q should contain three columns. Row %q was parsed into %q", cmd, row, fields)
		if fields[0] != distroName {
			continue
		}
		t.Log("OK!")
		return fields[1]
	}
	return "Unregistered"
}