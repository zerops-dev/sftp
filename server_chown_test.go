//go:build linux
// +build linux

package sftp

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func chownClientServerPair(t *testing.T) *Client {
	t.Helper()

	uid, gid := os.Getuid(), os.Getgid()
	if uid <= 0 || gid <= 0 {
		t.Skip("WithServerChown needs a non-root uid and gid")
	}

	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	server, err := NewServer(struct {
		io.Reader
		io.WriteCloser
	}{sr, sw}, WithServerChown(uid, gid))
	require.NoError(t, err)
	go server.Serve()

	client, err := NewClientPipe(cr, cw)
	require.NoError(t, err)
	t.Cleanup(func() {
		// The server has to go first: the client waits for its side of the pipe to end.
		server.Close()
		client.Close()
	})
	return client
}

func ctimeOf(t *testing.T, name string) syscall.Timespec {
	t.Helper()

	var st syscall.Stat_t
	require.NoError(t, syscall.Lstat(name, &st))
	return st.Ctim
}

// existingFile creates a file and waits long enough,
// that any later change of its metadata is visible in its ctime.
func existingFile(t *testing.T) string {
	t.Helper()

	name := filepath.Join(t.TempDir(), "existing.txt")
	require.NoError(t, os.WriteFile(name, []byte("hello"), 0o640))
	time.Sleep(50 * time.Millisecond)
	return name
}

func TestServerChownDoesNotTouchExistingFile(t *testing.T) {
	tests := []struct {
		name  string
		flags int
	}{
		{"read", os.O_RDONLY},
		{"write", os.O_WRONLY},
		{"write create", os.O_WRONLY | os.O_CREATE},
		{"read write create", os.O_RDWR | os.O_CREATE},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := chownClientServerPair(t)
			name := existingFile(t)
			before := ctimeOf(t, name)

			f, err := client.OpenFile(name, tt.flags)
			require.NoError(t, err)
			require.NoError(t, f.Close())

			require.Equal(t, before, ctimeOf(t, name), "open must not change the ctime of an existing file")
		})
	}
}

func TestServerChownCreatesFile(t *testing.T) {
	client := chownClientServerPair(t)
	name := filepath.Join(t.TempDir(), "new.txt")

	f, err := client.OpenFile(name, os.O_WRONLY|os.O_CREATE)
	require.NoError(t, err)
	_, err = f.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	var st syscall.Stat_t
	require.NoError(t, syscall.Lstat(name, &st))
	require.Equal(t, uint32(os.Getuid()), st.Uid)
	require.Equal(t, uint32(os.Getgid()), st.Gid)

	got, err := os.ReadFile(name)
	require.NoError(t, err)
	require.Equal(t, "hello", string(got))
}

func TestServerChownExclOnExistingFile(t *testing.T) {
	client := chownClientServerPair(t)
	name := existingFile(t)
	before := ctimeOf(t, name)

	_, err := client.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	require.Error(t, err)

	require.Equal(t, before, ctimeOf(t, name))
}

func requireOwnedByUs(t *testing.T, name string) {
	t.Helper()

	var st syscall.Stat_t
	require.NoError(t, syscall.Lstat(name, &st))
	require.Equal(t, uint32(os.Getuid()), st.Uid)
	require.Equal(t, uint32(os.Getgid()), st.Gid)
}

func TestServerOpenfileCreatedThroughDanglingSymlink(t *testing.T) {
	tests := []struct {
		name string
		// links maps each symlink to its target, both relative to the test directory
		links    map[string]string
		absolute bool
		open     string
		target   string
	}{
		{name: "relative", links: map[string]string{"link": "target.txt"}, open: "link", target: "target.txt"},
		{name: "absolute", links: map[string]string{"link": "target.txt"}, absolute: true, open: "link", target: "target.txt"},
		{name: "other directory", links: map[string]string{"sub/link": "../target.txt"}, open: "sub/link", target: "target.txt"},
		{name: "chain", links: map[string]string{"a": "b", "b": "sub/c", "sub/c": "../target.txt"}, open: "a", target: "target.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
			for link, target := range tt.links {
				if tt.absolute {
					target = filepath.Join(dir, target)
				}
				require.NoError(t, os.Symlink(target, filepath.Join(dir, link)))
			}

			svr := &Server{chownUID: 1, chownGID: 1}
			f, created, err := svr.openfileCreated(filepath.Join(dir, tt.open), os.O_WRONLY|os.O_CREATE, 0o644)
			require.NoError(t, err)
			require.NoError(t, f.Close())
			require.True(t, created, "a file created through a dangling symlink must be reported as created")

			fi, err := os.Lstat(filepath.Join(dir, tt.target))
			require.NoError(t, err)
			require.True(t, fi.Mode().IsRegular())

			// The second open finds the file, so it must not report it as created.
			f, created, err = svr.openfileCreated(filepath.Join(dir, tt.open), os.O_WRONLY|os.O_CREATE, 0o644)
			require.NoError(t, err)
			require.NoError(t, f.Close())
			require.False(t, created)
		})
	}
}

func TestServerOpenfileCreatedSymlinkLoop(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Symlink("b", filepath.Join(dir, "a")))
	require.NoError(t, os.Symlink("a", filepath.Join(dir, "b")))

	svr := &Server{chownUID: 1, chownGID: 1}
	_, _, err := svr.openfileCreated(filepath.Join(dir, "a"), os.O_WRONLY|os.O_CREATE, 0o644)
	require.ErrorIs(t, err, syscall.ELOOP, "the kernel reports the loop on the non-creating open")
}

func TestServerChownCreatesFileThroughDanglingSymlink(t *testing.T) {
	client := chownClientServerPair(t)
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	target := filepath.Join(dir, "target.txt")
	require.NoError(t, os.Symlink("target.txt", link))

	f, err := client.OpenFile(link, os.O_WRONLY|os.O_CREATE)
	require.NoError(t, err)
	_, err = f.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	requireOwnedByUs(t, target)

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "hello", string(got))
}
