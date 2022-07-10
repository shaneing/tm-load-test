package loadtest

import (
	"context"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"strings"
	"time"

	"github.com/informalsystems/tm-load-test/internal/logging"
)

// ExecuteStandalone will run a standalone (non-coordinator/worker) load test.
func ExecuteStandalone(cfg Config) error {
	logger := logging.NewLogrusLogger("loadtest")

	// if we need to wait for the network to stabilize first
	if cfg.ExpectPeers > 0 {
		peers, err := waitForTendermintNetworkPeers(
			cfg.Endpoints,
			cfg.EndpointSelectMethod,
			cfg.ExpectPeers,
			cfg.MinConnectivity,
			cfg.MaxEndpoints,
			time.Duration(cfg.PeerConnectTimeout)*time.Second,
			logger,
		)
		if err != nil {
			logger.Error("Failed while waiting for peers to connect", "err", err)
			return err
		}
		cfg.Endpoints = peers
	}

	logger.Info("Connecting to remote endpoints")
	tg := NewTransactorGroup()
	if err := tg.AddAll(&cfg); err != nil {
		return err
	}
	logger.Info("Initiating load test")

	remote := strings.Replace(cfg.Endpoints[0], "ws", "tcp", 1)
	client, err := rpchttp.New(remote, "/websocket")
	if err != nil {
		logger.Error("Failed to new rpc", "err", err, "remote", remote)
		return err
	}

	err = client.Start()
	if err != nil {
		logger.Error("Failed to start client", "err", err)
		return err
	}

	defer func() {
		_ = client.Stop()
	}()

	tg.Start()

	prevStatus, err := client.Status(context.Background())
	if err != nil {
		logger.Error("Failed to get prev status", "err", err)
		return err
	}

	var cancelTrap chan struct{}
	if !cfg.NoTrapInterrupts {
		// we want to know if the user hits Ctrl+Break
		cancelTrap = trapInterrupts(func() { tg.Cancel() }, logger)
		defer close(cancelTrap)
	} else {
		logger.Debug("Skipping trapping of interrupts (e.g. Ctrl+Break)")
	}

	if err := tg.Wait(); err != nil {
		logger.Error("Failed to execute load test", "err", err)
		return err
	}

	curStatus, err := client.Status(context.Background())
	if err != nil {
		logger.Error("Failed to get current status", "err", err)
		return err
	}

	stats := AggregateStats{
		TotalTxs:         tg.totalTxs(),
		TotalTimeSeconds: time.Since(tg.getStartTime()).Seconds(),
		TotalBytes:       tg.totalBytes(),
	}

	intervalsGenerator := func(s, e, interval int64) [][]int64 {
		var intervals [][]int64
		l := s
		for l <= e {
			r := l + interval - 1
			if r > e {
				r = e
			}
			intervals = append(intervals, []int64{l, r})
			l = r + 1
		}
		return intervals
	}

	var numTxsInBlock int
	logger.Info("Query block", "from", prevStatus.SyncInfo.LatestBlockHeight, "to", curStatus.SyncInfo.LatestBlockHeight)
	for _, interval := range intervalsGenerator(prevStatus.SyncInfo.LatestBlockHeight, curStatus.SyncInfo.LatestBlockHeight, 20) {
		info, err := client.BlockchainInfo(context.Background(), interval[0], interval[1])
		if err != nil {
			logger.Error("Failed to get info of block chain", "err", err, "from", interval[0], "to", interval[1])
			return err
		}
		for _, meta := range info.BlockMetas {
			numTxsInBlock += meta.NumTxs
		}
	}

	stats.TotalTxsInBlock = numTxsInBlock

	// if we need to write the final statistics
	if len(cfg.StatsOutputFile) > 0 {
		logger.Info("Writing aggregate statistics", "outputFile", cfg.StatsOutputFile)
		if err := writeAggregateStats(cfg.StatsOutputFile, stats); err != nil {
			logger.Error("Failed to write aggregate statistics", "err", err)
			return err
		}
	}

	logger.Info("Load test complete!")
	return nil
}
