package bbolt

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/projecteru2/docker-cni/config"
	"github.com/projecteru2/docker-cni/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestStore creates a new Store with a temporary database file.
// It returns the store and a cleanup function.
func setupTestStore(t *testing.T) (*Store, func()) {
	tmpDir, err := os.MkdirTemp("", "bbolt-test-*")
	require.NoError(t, err)

	conf := config.Config{
		StoreFile: filepath.Join(tmpDir, "test.db"),
	}

	s := New(conf)
	err = s.Open()
	require.NoError(t, err)

	cleanup := func() {
		s.Close()
		os.RemoveAll(tmpDir)
	}

	return s, cleanup
}

func TestNew(t *testing.T) {
	conf := config.Config{
		StoreFile: "test.db",
	}
	s := New(conf)
	assert.NotNil(t, s)
	assert.Equal(t, conf, s.conf)
	assert.Nil(t, s.db)
}

func TestOpenClose(t *testing.T) {
	s, cleanup := setupTestStore(t)
	defer cleanup()

	// Test double open
	err := s.Open()
	assert.NoError(t, err, "second open should not error")

	// Test close
	err = s.Close()
	assert.NoError(t, err)

	// Test double close
	err = s.Close()
	assert.NoError(t, err)

	// Test reopen after close
	err = s.Open()
	assert.NoError(t, err)
}

func TestContainerState(t *testing.T) {
	s, cleanup := setupTestStore(t)
	defer cleanup()

	testState := &specs.State{
		Version: "1.0.0",
		ID:      "container1",
		Status:  "running",
		Pid:     1234,
		Bundle:  "/path/to/bundle",
	}

	// Test PutContainerState
	err := s.PutContainerState(testState.ID, testState)
	assert.NoError(t, err)

	// Test GetContainerState
	retrieved, err := s.GetContainerState(testState.ID)
	assert.NoError(t, err)
	assert.NotNil(t, retrieved)
	assert.Equal(t, testState.Version, retrieved.Version)
	assert.Equal(t, testState.ID, retrieved.ID)
	assert.Equal(t, testState.Status, retrieved.Status)
	assert.Equal(t, testState.Pid, retrieved.Pid)
	assert.Equal(t, testState.Bundle, retrieved.Bundle)

	// Test getting non-existent container
	retrieved, err = s.GetContainerState("non-existent")
	assert.NoError(t, err)
	assert.Nil(t, retrieved)

	// Test updating existing container
	testState.Status = "stopped"
	err = s.PutContainerState(testState.ID, testState)
	assert.NoError(t, err)

	retrieved, err = s.GetContainerState(testState.ID)
	assert.NoError(t, err)
	assert.Equal(t, "stopped", retrieved.Status)
}

func TestInterfaceInfo(t *testing.T) {
	s, cleanup := setupTestStore(t)
	defer cleanup()

	testInfo := &store.InterfaceInfo{
		IFName:     "eth0",
		HostIFName: "veth123",
		IPs:        []string{"10.0.0.2/24", "fd00::2/64"},
		Routes:     []string{"default via 10.0.0.1"},
	}

	// Test PutInterfaceInfo
	err := s.PutInterfaceInfo("container1", testInfo)
	assert.NoError(t, err)

	// Test GetInterfaceInfo
	retrieved, err := s.GetInterfaceInfo("container1")
	assert.NoError(t, err)
	assert.NotNil(t, retrieved)
	assert.Equal(t, testInfo.IFName, retrieved.IFName)
	assert.Equal(t, testInfo.HostIFName, retrieved.HostIFName)
	assert.Equal(t, testInfo.IPs, retrieved.IPs)
	assert.Equal(t, testInfo.Routes, retrieved.Routes)

	// Test getting non-existent interface
	retrieved, err = s.GetInterfaceInfo("non-existent")
	assert.NoError(t, err)
	assert.Nil(t, retrieved)

	// Test updating existing interface info
	testInfo.IPs = []string{"10.0.0.3/24"}
	err = s.PutInterfaceInfo("container1", testInfo)
	assert.NoError(t, err)

	retrieved, err = s.GetInterfaceInfo("container1")
	assert.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.3/24"}, retrieved.IPs)
}

func TestDeleteContainers(t *testing.T) {
	s, cleanup := setupTestStore(t)
	defer cleanup()

	// Create test data
	containers := map[string]*specs.State{
		"container1": {ID: "container1", Status: "running", Pid: 1234},
		"container2": {ID: "container2", Status: "running", Pid: 5678},
		"container3": {ID: "container3", Status: "running", Pid: 9012},
	}

	// Add containers and interface info
	for id, state := range containers {
		err := s.PutContainerState(id, state)
		require.NoError(t, err)

		info := &store.InterfaceInfo{
			IFName:     "eth0",
			HostIFName: "veth" + id,
			IPs:        []string{"10.0.0.2/24"},
			Routes:     []string{"default via 10.0.0.1"},
		}
		err = s.PutInterfaceInfo(id, info)
		require.NoError(t, err)
	}

	// Test deletion
	existingContainers := map[string]struct{}{
		"container1": {},
		"container3": {},
	}

	deleted, err := s.DeleteContiners(existingContainers)
	assert.NoError(t, err)
	assert.Len(t, deleted, 1)

	// Verify container2 was deleted
	state, exists := deleted["container2"]
	assert.True(t, exists)
	assert.Equal(t, containers["container2"].Status, state.Status)
	assert.Equal(t, containers["container2"].Pid, state.Pid)

	// Verify remaining containers still exist
	for _, id := range []string{"container1", "container3"} {
		state, err := s.GetContainerState(id)
		assert.NoError(t, err)
		assert.NotNil(t, state)

		info, err := s.GetInterfaceInfo(id)
		assert.NoError(t, err)
		assert.NotNil(t, info)
	}

	// Verify deleted container's data is gone
	state2, err := s.GetContainerState("container2")
	assert.NoError(t, err)
	assert.Nil(t, state2)

	info2, err := s.GetInterfaceInfo("container2")
	assert.NoError(t, err)
	assert.Nil(t, info2)
}
