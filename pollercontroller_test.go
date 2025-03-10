package gocbcore

import (
	"time"
	"unsafe"

	"github.com/couchbase/gocbcore/v10/memd"
)

// This test tests that after calling stop then force http poller will not attempt to do work.
func (suite *UnitTestSuite) TestPollerControllerForceHTTPAndStopRace() {
	ccp := &cccpConfigController{
		looperStopSig: make(chan struct{}),
		looperDoneSig: make(chan struct{}),
	}

	cliMux := &httpClientMux{
		mgmtEpList: []routeEndpoint{
			{
				Address: "localhost:8091",
			},
		},
	}
	htt := &httpConfigController{
		baseHTTPConfigController: &baseHTTPConfigController{
			looperStopSig: make(chan struct{}),
			looperDoneSig: make(chan struct{}),
		},
		muxer: &httpMux{
			muxPtr: unsafe.Pointer(cliMux),
		},
	}

	poller := newPollerController(ccp, htt, &configManagementComponent{}, func(err error) bool {
		return false
	})
	poller.activeController = ccp

	poller.Stop()
	poller.ForceHTTPPoller()

	suite.Assert().Equal(poller.cccpPoller, poller.activeController)
}

// This test tests the scenario where ForceHTTPPoller and OnNewRouteConfig deadlock.
// This can happen when there are 2+ connections and one successfully bootstraps whilst the
// other fails with bucket not found. If cccp successfully gets a config at the same time
// as the bucket not found connection returns up the stack to ForceHTTPPoller then a deadlock
// can occur where ForceHTTPPoller is waiting for cccp to complete but cccp is blocking by waiting for
// the controllerLock lock in OnNewRouteConfig, which is already held by ForceHTTPPoller.
func (suite *UnitTestSuite) TestPollerControllerForceHTTPAndNewConfig() {
	config, err := suite.LoadRawTestDataset("bucket_config_with_external_addresses")
	suite.Require().Nil(err)

	pipeline := newPipeline(routeEndpoint{Address: "127.0.0.1:11210"}, 1, 10, nil)
	muxer := new(mockDispatcher)
	muxer.On("PipelineSnapshot").Return(&pipelineSnapshot{
		state: &kvMuxState{
			routeCfg: routeConfig{
				revID:   1,
				bktType: bktTypeCouchbase,
			},
			pipelines: []*memdPipeline{
				pipeline,
			},
		},
		idx: 0,
	}, nil)

	cfgMgr := &configManagementComponent{
		currentConfig: &routeConfig{
			revID: -1,
		},
	}

	ccp := &cccpConfigController{
		looperStopSig:      make(chan struct{}),
		looperDoneSig:      make(chan struct{}),
		cfgMgr:             cfgMgr,
		muxer:              muxer,
		confCccpPollPeriod: 10 * time.Second,
		confCccpMaxWait:    5 * time.Second,
		isFallbackErrorFn: func(err error) bool {
			return false
		},
	}
	cliMux := &httpClientMux{
		mgmtEpList: []routeEndpoint{
			{
				Address: "localhost:8091",
			},
		},
	}
	htt := &httpConfigController{
		baseHTTPConfigController: &baseHTTPConfigController{
			looperStopSig: make(chan struct{}),
			looperDoneSig: make(chan struct{}),
		},
		muxer: &httpMux{
			muxPtr: unsafe.Pointer(cliMux),
		},
	}

	poller := newPollerController(ccp, htt, cfgMgr, func(err error) bool {
		return false
	})
	poller.activeController = ccp

	go ccp.DoLoop()
	c := &memdOpConsumer{
		parent:   pipeline.queue,
		isClosed: false,
	}
	req := pipeline.queue.pop(c)
	suite.Require().Equal(req.Command, memd.CmdGetClusterConfig)
	req.tryCallback(&memdQResponse{
		Packet: &memd.Packet{
			Value: config,
		},
	}, nil)

	poller.ForceHTTPPoller()
	// Let ForceHTTPPoller take the lock, have hit the cccp poller done channel, and restarted cccp.
	time.Sleep(50 * time.Millisecond)

	// This will hang if there's a deadlock between ForceHTTPPoller and OnNewRouteConfig within the poller controller.
	suite.Assert().Nil(poller.PollerError())

	// The ForceHTTPPoller should pick up that the poller has seen a config whilst it was waiting on the cccp done
	// channel and start cccp back up again.
	suite.Assert().Equal(poller.cccpPoller, poller.activeController)

	poller.Stop()

	select {
	case <-poller.Done():
	case <-time.After(2 * time.Second):
		suite.T().Fatalf("Poller controller did not halt in required time")
	}
}
