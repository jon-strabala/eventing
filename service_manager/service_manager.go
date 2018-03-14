package servicemanager

import (
	"bytes"
	"os"
	"time"

	"github.com/couchbase/cbauth/service"
	"github.com/couchbase/eventing/logging"
	"github.com/couchbase/eventing/util"
)

// GetNodeInfo callback for cbauth service.Manager
func (m *ServiceMgr) GetNodeInfo() (*service.NodeInfo, error) {
	logPrefix := "ServiceMgr::GetNodeInfo"

	logging.Infof("%s nodeInfo: %#v", logPrefix, m.nodeInfo)
	return m.nodeInfo, nil
}

// Shutdown callback for cbauth service.Manager
func (m *ServiceMgr) Shutdown() error {
	logging.Infof("ServiceMgr::Shutdown")

	os.Exit(0)

	return nil
}

// GetTaskList callback for cbauth service.Manager
func (m *ServiceMgr) GetTaskList(rev service.Revision, cancel service.Cancel) (*service.TaskList, error) {
	logPrefix := "ServiceMgr::GetTaskList"

	logging.Infof("%s rev: %#v", logPrefix, rev)

	state, err := m.wait(rev, cancel)
	if err != nil {
		return nil, err
	}

	taskList := stateToTaskList(state)
	logging.Debugf("%s tasklist: %#v", logPrefix, taskList)

	return taskList, nil
}

// CancelTask callback for cbauth service.Manager
func (m *ServiceMgr) CancelTask(id string, rev service.Revision) error {
	logPrefix := "ServiceMgr::CancelTask"

	m.mu.Lock()
	defer m.mu.Unlock()

	logging.Infof("%s id: %s rev: %#v", logPrefix, id, rev)

	tasks := stateToTaskList(m.state).Tasks
	task := (*service.Task)(nil)

	for i := range tasks {
		t := &tasks[i]

		if t.ID == id {
			task = t
			break
		}
	}

	if task == nil {
		return service.ErrNotFound
	}

	if !task.IsCancelable {
		return service.ErrNotSupported
	}

	if rev != nil && !bytes.Equal(rev, task.Rev) {
		return service.ErrConflict
	}

	return m.cancelActualTaskLocked(task)
}

// GetCurrentTopology callback for cbauth service.Manager
func (m *ServiceMgr) GetCurrentTopology(rev service.Revision, cancel service.Cancel) (*service.Topology, error) {
	logPrefix := "ServiceMgr::GetCurrentTopology"

	logging.Infof("%s rev: %#v", logPrefix, rev)

	state, err := m.wait(rev, cancel)
	if err != nil {
		return nil, err
	}

	topology := m.stateToTopology(state)
	logging.Debugf("%s topology: %#v", logPrefix, topology)

	return topology, nil

}

// PrepareTopologyChange callback for cbauth service.Manager
func (m *ServiceMgr) PrepareTopologyChange(change service.TopologyChange) error {
	logPrefix := "ServiceMgr::PrepareTopologyChange"

	m.mu.Lock()
	defer m.mu.Unlock()

	logging.Infof("%s change: %#v", logPrefix, change)

	m.keepNodeUUIDs = make([]string, 0)

	for _, node := range change.KeepNodes {
		m.keepNodeUUIDs = append(m.keepNodeUUIDs, string(node.NodeInfo.NodeID))
	}

	logging.Infof("%s keepNodeUUIDs: %v", logPrefix, m.keepNodeUUIDs)

	m.updateStateLocked(func(s *state) {
		m.rebalanceID = change.ID
	})

	m.superSup.NotifyPrepareTopologyChange(m.keepNodeUUIDs)

	return nil
}

// StartTopologyChange callback for cbauth service.Manager
func (m *ServiceMgr) StartTopologyChange(change service.TopologyChange) error {
	logPrefix := "ServiceMgr::StartTopologyChange"

	m.mu.Lock()
	defer m.mu.Unlock()

	logging.Infof("%s change: %#v", logPrefix, change)

	if m.state.rebalanceID != change.ID || m.rebalancer != nil {
		logging.Errorf("%s Returning errConflict, rebalanceID: %v change id: %v rebalancer dump: %#v",
			logPrefix, m.state.rebalanceID, change.ID, m.rebalancer)
		return service.ErrConflict
	}

	if change.CurrentTopologyRev != nil {
		haveRev := decodeRev(change.CurrentTopologyRev)
		if haveRev != m.state.rev {
			logging.Errorf("%s Returning errConflict, state rev: %v haveRev: %v",
				logPrefix, m.state.rev, haveRev)
			return service.ErrConflict
		}
	}

	ctx := &rebalanceContext{
		rev:    0,
		change: change,
	}

	m.rebalanceCtx = ctx

	switch change.Type {
	case service.TopologyChangeTypeFailover:
		util.Retry(util.NewFixedBackoff(time.Second), storeKeepNodesCallback, m.keepNodeUUIDs)
		m.failoverNotif = true

	case service.TopologyChangeTypeRebalance:

		nodeAddrs, err := m.getActiveNodeAddrs()

		if len(nodeAddrs) > 0 && err == nil {

			logging.Infof("%s Querying nodes: %r for bootstrap status", logPrefix, nodeAddrs)

			// Fail rebalance if some apps are undergoing bootstrap
			appsBootstrapping, err := util.GetAggBootstrappingApps("/getBootstrappingApps", nodeAddrs)
			logging.Infof("%s Status of app bootstrap across all Eventing nodes: %v", logPrefix, appsBootstrapping)
			if err != nil {
				logging.Warnf("%s Some apps are undergoing bootstrap on some/all Eventing nodes, err: %v", logPrefix, err)
				return err
			}
		}

		util.Retry(util.NewFixedBackoff(time.Second), storeKeepNodesCallback, m.keepNodeUUIDs)

		m.startRebalance(change)

		logging.Infof("%s Starting up rebalancer", logPrefix)

		rebalancer := newRebalancer(m.adminHTTPPort, change, m.rebalanceDoneCallback, m.rebalanceProgressCallback,
			m.keepNodeUUIDs)
		m.rebalancer = rebalancer

	default:
		return service.ErrNotSupported
	}

	return nil
}
