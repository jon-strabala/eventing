package producer

import (
	"fmt"
	"time"

	"github.com/couchbase/eventing/util"
	"github.com/couchbase/indexing/secondary/common"
	"github.com/couchbase/indexing/secondary/logging"
)

func (p *Producer) watchClusterChanges() {
	selfRestart := func() {
		logging.Infof("PWCC[%s:%d] Observed change in cluster state", p.appName, p.LenRunningConsumers())
		time.Sleep(WatchClusterChangeInterval)
		go p.watchClusterChanges()
	}

	hostaddr := fmt.Sprintf("127.0.0.1:%s", p.nsServerPort)

	localAddress, err := util.LocalEventingServiceHost(p.auth, hostaddr)
	if err != nil {
		logging.Errorf("PWCC[%s:%d] Failed to get address for local eventing node, err: %v", p.appName, p.LenRunningConsumers(), err)
	}

	clusterURL := fmt.Sprintf("http://%s@%s:%s", p.auth, localAddress, p.nsServerPort)

	scn, err := common.NewServicesChangeNotifier(clusterURL, "default")
	if err != nil {
		logging.Errorf("PWCC[%s:%d] Failed to get ChangeNotifier handle, err: %v", p.appName, p.LenRunningConsumers(), err)
		selfRestart()
		return
	}

	defer scn.Close()

	ch := scn.GetNotifyCh()

	for {
		select {
		case _, ok := <-ch:
			if !ok {
				selfRestart()
				return
			}
			logging.Infof("PWCC[%s:%d] Got message that cluster state has changed", p.appName, p.LenRunningConsumers())
			p.clusterStateChange <- true
			selfRestart()
			return
		}
	}

}
