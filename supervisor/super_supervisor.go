package supervisor

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/couchbase/cbauth/service"
	"github.com/couchbase/eventing/common"
	"github.com/couchbase/eventing/producer"
	"github.com/couchbase/eventing/suptree"
	"github.com/couchbase/eventing/util"
	"github.com/couchbase/indexing/secondary/logging"
)

// NewSuperSupervisor creates the super_supervisor handle
func NewSuperSupervisor(kvPort, restPort, uuid string) *SuperSupervisor {
	s := &SuperSupervisor{
		CancelCh: make(chan struct{}, 1),
		kvPort:   kvPort,
		producerSupervisorTokenMap: make(map[common.EventingProducer]suptree.ServiceToken),
		restPort:                   restPort,
		runningProducers:           make(map[string]common.EventingProducer),
		supCmdCh:                   make(chan supCmdMsg, 10),
		superSup:                   suptree.NewSimple("super_supervisor"),
		uuid:                       uuid,
	}
	go s.superSup.ServeBackground()

	go func(s *SuperSupervisor) {
		logging.Infof("SSUP: Registering against cbauth_service ")

		err := service.RegisterManager(s, nil)
		if err != nil {
			logging.Errorf("SSUP: Failed to register against cbauth_service, err: %v", err)
			return
		}
	}(s)

	return s
}

// EventHandlerLoadCallback is registered as callback from metakv observe calls on event handlers & settings path
func (s *SuperSupervisor) EventHandlerLoadCallback(path string, value []byte, rev interface{}) error {
	if value != nil {
		splitRes := strings.Split(path, "/")
		appName := splitRes[len(splitRes)-1]
		msg := supCmdMsg{
			ctx: appName,
			cmd: "load",
		}
		s.supCmdCh <- msg
	}
	return nil
}
func (s *SuperSupervisor) spawnApp(appName string) {
	metakvAppHostPortsPath := fmt.Sprintf("%s%s/", MetakvProducerHostPortsPath, appName)
	p := producer.NewProducer(appName, s.kvPort, metakvAppHostPortsPath, s.restPort, s.uuid)

	token := s.superSup.Add(p)
	s.runningProducers[appName] = p
	s.producerSupervisorTokenMap[p] = token

	err := util.RecursiveDelete(metakvAppHostPortsPath)
	if err != nil {
		logging.Fatalf("SSUP[%d] Failed to cleanup previous hostport addrs from metakv, err: %v", len(s.runningProducers), err)
		return
	}

	go func(p *producer.Producer, appName, metakvAppHostPortsPath string) {
		var err error
		p.ProducerListener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			logging.Fatalf("SSUP[%d] Listen failed with error: %v", len(s.runningProducers), err)
			return
		}

		addr := p.ProducerListener.Addr().String()
		logging.Infof("SSUP[%d] Listening on host string %s app: %s", len(s.runningProducers), addr, appName)
		err = util.MetakvSet(metakvAppHostPortsPath+addr, []byte(addr), nil)
		if err != nil {
			logging.Fatalf("SSUP[%d] Failed to store hostport for app: %s into metakv, err: %v", len(s.runningProducers), appName, err)
			return
		}

		h := http.NewServeMux()

		h.HandleFunc("/getAggRebalanceStatus", p.AggregateTaskProgress)
		h.HandleFunc("/getNodeMap", p.GetNodeMap)
		h.HandleFunc("/getRebalanceStatus", p.RebalanceStatus)
		h.HandleFunc("/getRemainingEvents", p.DcpEventsRemainingToProcess)
		h.HandleFunc("/getSettings", p.GetSettings)
		h.HandleFunc("/getVbStats", p.GetConsumerVbProcessingStats)
		h.HandleFunc("/getWorkerMap", p.GetWorkerMap)
		h.HandleFunc("/updateSettings", p.UpdateSettings)

		http.Serve(p.ProducerListener, h)
	}(p, appName, metakvAppHostPortsPath)
}

// HandleSupCmdMsg handles control commands like app (re)deploy, settings update
func (s *SuperSupervisor) HandleSupCmdMsg() {
	for {
		select {
		case msg := <-s.supCmdCh:
			appName := msg.ctx
			logging.Infof("SSUP[%d] Loading app: %s", len(s.runningProducers), appName)

			// Clean previous running instance of app producers
			if p, ok := s.runningProducers[appName]; ok {
				logging.Infof("SSUP[%d] App: %s, cleaning up previous running instance", len(s.runningProducers), appName)
				p.NotifyInit()

				s.superSup.Remove(s.producerSupervisorTokenMap[p])
				delete(s.producerSupervisorTokenMap, p)
				delete(s.runningProducers, appName)

				p.NotifySupervisor()
				logging.Infof("SSUP[%d] Cleaned up previous running producer instance, app: %s", len(s.runningProducers), appName)
			}

			s.spawnApp(appName)
		}
	}
}
