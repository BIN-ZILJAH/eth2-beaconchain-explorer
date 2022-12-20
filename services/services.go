package services

import (
	"encoding/hex"
	"encoding/json"
	"eth2-exporter/cache"
	"eth2-exporter/db"
	"eth2-exporter/price"
	"eth2-exporter/types"
	"eth2-exporter/utils"
	"fmt"
	"html/template"
	"math"
	"math/big"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	itypes "github.com/gobitfly/eth-rewards/types"
	"github.com/shopspring/decimal"

	"github.com/ethereum/go-ethereum/common"
	"github.com/sirupsen/logrus"

	geth_types "github.com/ethereum/go-ethereum/core/types"
	geth_rpc "github.com/ethereum/go-ethereum/rpc"
)

var eth1BlockDepositReached atomic.Value
var depositThresholdReached atomic.Value

var logger = logrus.New().WithField("module", "services")

// Init will initialize the services
func Init() {
	ready := &sync.WaitGroup{}
	ready.Add(1)
	go epochUpdater(ready)

	ready.Add(1)
	go slotUpdater(ready)

	ready.Add(1)
	go latestProposedSlotUpdater(ready)

	ready.Add(1)
	go latestBlockUpdater(ready)

	ready.Add(1)
	go slotVizUpdater(ready)

	ready.Add(1)
	go indexPageDataUpdater(ready)

	ready.Add(1)
	go poolsUpdater(ready)

	ready.Add(1)
	go relaysUpdater(ready)

	ready.Add(1)
	go chartsPageDataUpdater(ready)

	ready.Add(1)
	go statsUpdater(ready)

	ready.Add(1)
	go mempoolUpdater(ready)

	ready.Add(1)
	go burnUpdater(ready)

	ready.Add(1)
	go gasNowUpdater(ready)

	ready.Wait()
}

func InitNotifications() {
	logger.Infof("starting notifications-sender")
	go notificationsSender()
}

func getRelaysPageData() (*types.RelaysResp, error) {
	start := time.Now()
	defer func() {
		logger.WithFields(logrus.Fields{"duration": time.Since(start)}).Info("completed caching relays page data")
	}()
	var relaysData types.RelaysResp

	relaysData.LastUpdated = start

	overallStatsQuery, err := db.ReaderDb.Preparex(`
		with stats as (
			select 
				tag_id as relay_id,
				count(*) as block_count,
				sum(value) as total_value,
				ROUND(avg(value)) as avg_value,
				count(distinct builder_pubkey) as unique_builders,
				max(value) as max_value,
				(select rb2.block_slot from relays_blocks rb2 where rb2.value = max(rb.value) and rb2.tag_id = rb.tag_id limit 1) as max_value_slot
			from relays_blocks rb
			where 
				rb.block_slot > $1 and 
				rb.block_root not in (select bt.blockroot from blocks_tags bt where bt.tag_id='invalid-relay-reward') 
			group by tag_id 
		)
		select 
			tags.metadata ->> 'name' as "name",
			relays.public_link as link,
			relays.is_censoring as censors,
			relays.is_ethical as ethical,
			stats.block_count / (select max(blocks.slot) - $1 from blocks)::float as network_usage,
			stats.*
		from relays
		left join stats on stats.relay_id = relays.tag_id
		left join tags on tags.id = relays.tag_id 
		where stats.relay_id = tag_id 
		order by stats.block_count DESC`)
	if err != nil {
		logger.Errorf("failed to prepare overallStatsQuery: %v", err)
		return nil, err
	}
	dayInSlots := 24 * 60 * 60 / utils.Config.Chain.Config.SecondsPerSlot

	tmp := [3]types.RelayInfoContainer{{Days: 7}, {Days: 31}, {Days: 180}}
	for i := 0; i < len(tmp); i++ {
		tmp[i].IsFirst = i == 0
		err = overallStatsQuery.Select(&tmp[i].RelaysInfo, LatestSlot()-(tmp[i].Days*dayInSlots))
		if err != nil {
			return nil, err
		}
		// calculate total adoption
		for j := 0; j < len(tmp[i].RelaysInfo); j++ {
			tmp[i].NetworkParticipation += tmp[i].RelaysInfo[j].NetworkUsage
		}
	}
	relaysData.RelaysInfoContainers = tmp

	err = db.ReaderDb.Select(&relaysData.TopBuilders, `
		select 
			builder_pubkey,
			SUM(c) as c,
			jsonb_agg(tags.metadata) as tags,
			max(latest_slot) as latest_slot
		from (
			select 
				builder_pubkey,
				count(*) as c,
				tag_id,
				(
					select block_slot
					from relays_blocks rb2
					where
						rb2.builder_pubkey = rb.builder_pubkey
					order by block_slot desc
					limit 1
				) as latest_slot
			from (
				select * 
				from relays_blocks
				where block_slot > $1
				order by block_slot desc) rb
			group by builder_pubkey, tag_id 
		) foo
		left join tags on tags.id = foo.tag_id
		group by builder_pubkey 
		order by c desc`, LatestSlot()-(14*dayInSlots))
	if err != nil {
		logger.Errorf("failed to get builder ranking %v", err)
		return nil, err
	}

	err = db.ReaderDb.Select(&relaysData.RecentBlocks, `
		select
			jsonb_agg(tags.metadata order by id) as tags,
			max(relays_blocks.value) as value,
			relays_blocks.block_slot as slot,
			relays_blocks.builder_pubkey as builder_pubkey,
			relays_blocks.proposer_fee_recipient as proposer_fee_recipient,
			validators.validatorindex as proposer,
			encode(exec_extra_data, 'hex') as block_extra_data
		from (
			select blockroot, exec_extra_data
			from blocks
			where blockroot in (
				select rb.block_root
				from relays_blocks rb
			) 
			order by blocks.slot desc
			limit 15
		) as blocks
		left join relays_blocks
			on relays_blocks.block_root = blocks.blockroot
		left join tags 
			on tags.id = relays_blocks.tag_id 
		left join validators
			on validators.pubkey = relays_blocks.proposer_pubkey  
		group by 
			blockroot, 
			relays_blocks.block_slot,
			relays_blocks.builder_pubkey,
			relays_blocks.proposer_fee_recipient,
			blocks.exec_extra_data,
			validators.validatorindex 
		order by relays_blocks.block_slot desc`)
	if err != nil {
		logger.Errorf("failed to get latest blocks for relays page %v", err)
		return nil, err
	}

	err = db.ReaderDb.Select(&relaysData.TopBlocks, `
		select
			jsonb_agg(tags.metadata order by id) as tags,
			max(relays_blocks.value) as value,
			relays_blocks.block_slot as slot,
			relays_blocks.builder_pubkey as builder_pubkey,
			relays_blocks.proposer_fee_recipient as proposer_fee_recipient,
			validators.validatorindex as proposer,
			encode(exec_extra_data, 'hex') as block_extra_data
		from (
			select * 
			from relays_blocks
			where relays_blocks.block_root not in (select bt.blockroot from blocks_tags bt where bt.tag_id='invalid-relay-reward') 
			order by relays_blocks.value desc
			limit 15
		) as relays_blocks 
		left join blocks
			on relays_blocks.block_root = blocks.blockroot
		left join tags 
			on tags.id = relays_blocks.tag_id 
		left join validators
			on validators.pubkey = relays_blocks.proposer_pubkey  
		group by 
			blockroot, 
			relays_blocks.block_slot,
			relays_blocks.builder_pubkey,
			relays_blocks.proposer_fee_recipient,
			blocks.exec_fee_recipient,
			blocks.exec_extra_data,
			validators.validatorindex 
		order by value desc`)
	if err != nil {
		logger.Errorf("failed to get top blocks for relays page %v", err)
		return nil, err
	}

	return &relaysData, nil
}

func relaysUpdater(wg *sync.WaitGroup) {
	firstRun := true

	for {
		data, err := getRelaysPageData()
		if err != nil {
			logger.Errorf("error retrieving relays page data: %v", err)
			time.Sleep(time.Second * 10)
			continue
		}

		cacheKey := fmt.Sprintf("%d:frontend:relaysData", utils.Config.Chain.Config.DepositChainID)
		err = cache.TieredCache.Set(cacheKey, data, time.Hour*24)
		if err != nil {
			logger.Errorf("error caching relaysData: %v", err)
		}
		if firstRun {
			logger.Info("initialized relays page updater")
			wg.Done()
			firstRun = false
		}
		ReportStatus("relaysUpdater", "Running", nil)
		time.Sleep(time.Minute)
	}
}

func epochUpdater(wg *sync.WaitGroup) {
	firstRun := true
	for {
		// latest epoch acording to the node
		var epochNode uint64
		err := db.WriterDb.Get(&epochNode, "SELECT headepoch FROM network_liveness order by headepoch desc LIMIT 1")
		if err != nil {
			logger.Errorf("error retrieving latest node epoch from the database: %v", err)
		} else {
			cacheKey := fmt.Sprintf("%d:frontend:latestNodeEpoch", utils.Config.Chain.Config.DepositChainID)
			err := cache.TieredCache.SetUint64(cacheKey, epochNode, time.Hour*24)
			if err != nil {
				logger.Errorf("error caching latestNodeEpoch: %v", err)
			}
		}

		// latest finalized epoch acording to the node
		var latestNodeFinalized uint64
		err = db.WriterDb.Get(&latestNodeFinalized, "SELECT finalizedepoch FROM network_liveness order by headepoch desc LIMIT 1")
		if err != nil {
			logger.Errorf("error retrieving latest node finalized epoch from the database: %v", err)
		} else {
			cacheKey := fmt.Sprintf("%d:frontend:latestNodeFinalizedEpoch", utils.Config.Chain.Config.DepositChainID)
			err := cache.TieredCache.SetUint64(cacheKey, latestNodeFinalized, time.Hour*24)
			if err != nil {
				logger.Errorf("error caching latestNodeFinalized: %v", err)
			}
		}

		// latest exported epoch
		var epoch uint64
		err = db.WriterDb.Get(&epoch, "SELECT COALESCE(MAX(epoch), 0) FROM blocks")
		if err != nil {
			logger.Errorf("error retrieving latest exported epoch from the database: %v", err)
		} else {
			cacheKey := fmt.Sprintf("%d:frontend:latestEpoch", utils.Config.Chain.Config.DepositChainID)
			err := cache.TieredCache.SetUint64(cacheKey, epoch, time.Hour*24)
			if err != nil {
				logger.Errorf("error caching latestEpoch: %v", err)
			}
		}

		// latest exportered finalized epoch
		var latestFinalized uint64
		err = db.WriterDb.Get(&latestFinalized, "SELECT COALESCE(MAX(epoch), 0) FROM epochs where finalized is true")
		if err != nil {
			logger.Errorf("error retrieving latest exported finalized epoch from the database: %v", err)
		} else {
			cacheKey := fmt.Sprintf("%d:frontend:latestFinalized", utils.Config.Chain.Config.DepositChainID)
			err := cache.TieredCache.SetUint64(cacheKey, latestFinalized, time.Hour*24)
			if err != nil {
				logger.Errorf("error caching latestFinalizedEpoch: %v", err)
			}
			if firstRun {
				logger.Info("initialized epoch updater")
				wg.Done()
				firstRun = false
			}
		}
		ReportStatus("epochUpdater", "Running", nil)
		time.Sleep(time.Second)
	}
}

func slotUpdater(wg *sync.WaitGroup) {
	firstRun := true

	for {
		var slot uint64
		err := db.WriterDb.Get(&slot, "SELECT COALESCE(MAX(slot), 0) FROM blocks where slot < $1", utils.TimeToSlot(uint64(time.Now().Add(time.Second*10).Unix())))

		if err != nil {
			logger.Errorf("error retrieving latest slot from the database: %v", err)

			if err.Error() == "sql: database is closed" {
				logger.Fatalf("error retrieving latest slot from the database: %v", err)
			}
		} else {
			cacheKey := fmt.Sprintf("%d:frontend:slot", utils.Config.Chain.Config.DepositChainID)
			err := cache.TieredCache.SetUint64(cacheKey, slot, time.Hour*24)
			if err != nil {
				logger.Errorf("error caching slot: %v", err)
			}
			if firstRun {
				logger.Info("initialized slot updater")
				wg.Done()
				firstRun = false
			}
		}
		ReportStatus("slotUpdater", "Running", nil)
		time.Sleep(time.Second)
	}
}

func poolsUpdater(wg *sync.WaitGroup) {
	firstRun := true

	for {
		data, err := getPoolsPageData()
		if err != nil {
			logger.Errorf("error retrieving pools page data: %v", err)
			time.Sleep(time.Second * 10)
			continue
		}

		cacheKey := fmt.Sprintf("%d:frontend:poolsData", utils.Config.Chain.Config.DepositChainID)
		err = cache.TieredCache.Set(cacheKey, data, time.Hour*24)
		if err != nil {
			logger.Errorf("error caching poolsData: %v", err)
		}
		if firstRun {
			logger.Info("initialized pools page updater")
			wg.Done()
			firstRun = false
		}
		ReportStatus("poolsUpdater", "Running", nil)
		time.Sleep(time.Minute * 10)
	}
}

func getPoolsPageData() (*types.PoolsResp, error) {
	var poolData types.PoolsResp
	err := db.ReaderDb.Select(&poolData.PoolInfos, `
	select pool as name, validators as count, apr * 100 as avg_performance_1d, (select avg(apr) from historical_pool_performance as hpp1 where hpp1.pool = hpp.pool AND hpp1.day > hpp.day - 7) * 100 as avg_performance_7d, (select avg(apr) from historical_pool_performance as hpp1 where hpp1.pool = hpp.pool AND hpp1.day > hpp.day - 31) * 100 as avg_performance_31d from historical_pool_performance hpp where day = (select max(day) from historical_pool_performance) order by validators desc;
	`)

	if err != nil {
		return nil, err
	}

	ethstoreData := &types.PoolInfo{}
	err = db.ReaderDb.Get(ethstoreData, `
	select 'ETH.STORE' as name, -1 as count, apr * 100 as avg_performance_1d, (select avg(apr) from eth_store_stats as e1 where e1.validator = -1 AND e1.day > e.day - 7) * 100 as avg_performance_7d, (select avg(apr) from eth_store_stats as e1 where e1.validator = -1 AND e1.day > e.day - 31) * 100 as avg_performance_31d from eth_store_stats e where day = (select max(day) from eth_store_stats) LIMIT 1;
	`)
	if err != nil {
		return nil, err
	}

	for _, pool := range poolData.PoolInfos {
		pool.EthstoreCompoarison1d = pool.AvgPerformance1d*100/ethstoreData.AvgPerformance1d - 100
		pool.EthstoreCompoarison7d = pool.AvgPerformance7d*100/ethstoreData.AvgPerformance7d - 100
		pool.EthstoreCompoarison31d = pool.AvgPerformance31d*100/ethstoreData.AvgPerformance31d - 100
	}
	poolData.PoolInfos = append([]*types.PoolInfo{ethstoreData}, poolData.PoolInfos...)

	return &poolData, nil
}

func latestProposedSlotUpdater(wg *sync.WaitGroup) {
	firstRun := true

	for {
		var slot uint64
		err := db.WriterDb.Get(&slot, "SELECT COALESCE(MAX(slot), 0) FROM blocks WHERE status = '1'")

		if err != nil {
			logger.Errorf("error retrieving latest proposed slot from the database: %v", err)
		} else {

			cacheKey := fmt.Sprintf("%d:frontend:latestProposedSlot", utils.Config.Chain.Config.DepositChainID)
			err = cache.TieredCache.SetUint64(cacheKey, slot, time.Hour*24)
			if err != nil {
				logger.Errorf("error caching latestProposedSlot: %v", err)
			}
			if firstRun {
				logger.Info("initialized last proposed slot updater")
				wg.Done()
				firstRun = false
			}
		}
		ReportStatus("latestProposedSlotUpdater", "Running", nil)
		time.Sleep(time.Second)
	}
}

func indexPageDataUpdater(wg *sync.WaitGroup) {
	firstRun := true

	for {
		logger.Infof("updating index page data")
		start := time.Now()
		data, err := getIndexPageData()
		if err != nil {
			logger.Errorf("error retrieving index page data: %v", err)
			time.Sleep(time.Second * 10)
			continue
		}
		logger.Infof("index page data update completed in %v", time.Since(start))

		cacheKey := fmt.Sprintf("%d:frontend:indexPageData", utils.Config.Chain.Config.DepositChainID)
		err = cache.TieredCache.Set(cacheKey, data, time.Hour*24)
		if err != nil {
			logger.Errorf("error caching indexPageData: %v", err)
		}
		if firstRun {
			logger.Info("initialized index page updater")
			wg.Done()
			firstRun = false
		}
		ReportStatus("indexPageDataUpdater", "Running", nil)
		time.Sleep(time.Second * 10)
	}
}

func slotVizUpdater(wg *sync.WaitGroup) {
	firstRun := true

	for {
		latestEpoch := LatestEpoch()
		if latestEpoch > 0 {
			epochData, err := db.GetSlotVizData(latestEpoch)
			if err != nil {
				logger.Errorf("error retrieving slot viz data from database: %v latest epoch: %v", err, latestEpoch)
			} else {
				cacheKey := fmt.Sprintf("%d:frontend:slotVizMetrics", utils.Config.Chain.Config.DepositChainID)
				err = cache.TieredCache.Set(cacheKey, epochData, time.Hour*24)
				if err != nil {
					logger.Errorf("error caching slotVizMetrics: %v", err)
				}
				if firstRun {
					logger.Info("initialized slotViz metrics")
					wg.Done()
					firstRun = false
				}
			}
		}
		ReportStatus("slotVizUpdater", "Running", nil)
		time.Sleep(time.Second)
	}
}

func getIndexPageData() (*types.IndexPageData, error) {
	currency := "ETH"

	data := &types.IndexPageData{}
	data.Mainnet = utils.Config.Chain.Config.ConfigName == "mainnet"
	data.NetworkName = utils.Config.Chain.Config.ConfigName
	data.DepositContract = utils.Config.Indexer.Eth1DepositContractAddress

	var epoch uint64
	err := db.ReaderDb.Get(&epoch, "SELECT COALESCE(MAX(epoch), 0) FROM epochs")
	if err != nil {
		return nil, fmt.Errorf("error retrieving latest epoch from the database: %v", err)
	}
	data.CurrentEpoch = epoch

	cutoffSlot := utils.TimeToSlot(uint64(time.Now().Add(time.Second * 10).Unix()))

	// If we are before the genesis block show the first 20 slots by default
	startSlotTime := utils.SlotToTime(0)
	genesisTransition := utils.SlotToTime(160)
	now := time.Now()

	// run deposit query until the Genesis period is over
	if now.Before(genesisTransition) || startSlotTime == time.Unix(0, 0) {
		if cutoffSlot < 15 {
			cutoffSlot = 15
		}
		type Deposit struct {
			Total   uint64    `db:"total"`
			BlockTs time.Time `db:"block_ts"`
		}

		deposit := Deposit{}
		err = db.ReaderDb.Get(&deposit, `
			SELECT COUNT(*) as total, COALESCE(MAX(block_ts),NOW()) AS block_ts
			FROM (
				SELECT publickey, SUM(amount) AS amount, MAX(block_ts) as block_ts
				FROM eth1_deposits
				WHERE valid_signature = true
				GROUP BY publickey
				HAVING SUM(amount) >= 32e9
			) a`)
		if err != nil {
			return nil, fmt.Errorf("error retrieving eth1 deposits: %v", err)
		}

		threshold, err := db.GetDepositThresholdTime()
		if err != nil {
			logger.WithError(err).Error("error could not calculate threshold time")
		}
		if threshold == nil {
			threshold = &deposit.BlockTs
		}

		data.DepositThreshold = float64(utils.Config.Chain.Config.MinGenesisActiveValidatorCount) * 32
		data.DepositedTotal = float64(deposit.Total) * 32

		data.ValidatorsRemaining = (data.DepositThreshold - data.DepositedTotal) / 32
		genesisDelay := time.Duration(int64(utils.Config.Chain.Config.GenesisDelay) * 1000 * 1000 * 1000) // convert seconds to nanoseconds

		minGenesisTime := time.Unix(int64(utils.Config.Chain.GenesisTimestamp), 0)

		data.MinGenesisTime = minGenesisTime.Unix()
		data.NetworkStartTs = minGenesisTime.Add(genesisDelay).Unix()

		if minGenesisTime.Before(time.Now()) {
			minGenesisTime = time.Now()
		}

		// enough deposits
		if data.DepositedTotal > data.DepositThreshold {
			if depositThresholdReached.Load() == nil {
				eth1BlockDepositReached.Store(*threshold)
				depositThresholdReached.Store(true)
			}
			eth1Block := eth1BlockDepositReached.Load().(time.Time)

			if !(startSlotTime == time.Unix(0, 0)) && eth1Block.Add(genesisDelay).After(minGenesisTime) {
				// Network starts after min genesis time
				data.NetworkStartTs = eth1Block.Add(genesisDelay).Unix()
			} else {
				data.NetworkStartTs = minGenesisTime.Unix()
			}
		}

		latestChartsPageData := LatestChartsPageData()
		if len(latestChartsPageData) != 0 {
			for _, c := range latestChartsPageData {
				if c.Path == "deposits" {
					data.DepositChart = c
				} else if c.Path == "deposits_distribution" {
					data.DepositDistribution = c
				}
			}
		}
	}
	if data.DepositChart != nil && data.DepositChart.Data != nil && data.DepositChart.Data.Series != nil {
		series := data.DepositChart.Data.Series
		if len(series) > 2 {
			points := series[1].Data.([][]float64)
			periodDays := float64(len(points))
			avgDepositPerDay := data.DepositedTotal / periodDays
			daysUntilThreshold := (data.DepositThreshold - data.DepositedTotal) / avgDepositPerDay
			estimatedTimeToThreshold := time.Now().Add(time.Hour * 24 * time.Duration(daysUntilThreshold))
			if estimatedTimeToThreshold.After(time.Unix(data.NetworkStartTs, 0)) {
				data.NetworkStartTs = estimatedTimeToThreshold.Add(time.Duration(int64(utils.Config.Chain.Config.GenesisDelay) * 1000 * 1000 * 1000)).Unix()
			}
		}
	}

	// has genesis occurred
	data.Genesis = now.After(startSlotTime)
	// show the transition view one hour before the first slot and until epoch 30 is reached
	data.GenesisPeriod = now.Add(time.Hour*24).After(startSlotTime) && now.Before(genesisTransition)

	if startSlotTime == time.Unix(0, 0) {
		data.Genesis = false
	}

	var scheduledCount uint8
	err = db.WriterDb.Get(&scheduledCount, `
		select count(*) from blocks where status = '0' and epoch = $1;
	`, epoch)
	if err != nil {
		return nil, fmt.Errorf("error retrieving scheduledCount from blocks: %v", err)
	}
	data.ScheduledCount = scheduledCount

	// get epochs from db
	var epochs []*types.IndexPageDataEpochs
	err = db.ReaderDb.Select(&epochs, `
		SELECT 
			epoch, 
			finalized, 
			globalparticipationrate, 
			votedether,

			eligibleether,
			validatorscount,
			averagevalidatorbalance
		FROM epochs 
		ORDER BY epochs DESC 
		LIMIT 10`)
	if err != nil {
		return nil, fmt.Errorf("error retrieving index epoch data: %v", err)
	}
	minEpoch := epochs[len(epochs)-1].Epoch

	if len(epochs) > 0 {
		for i := len(epochs) - 1; i >= 0; i-- {
			if epochs[i].Finalized {
				data.CurrentFinalizedEpoch = epochs[i].Epoch
				data.FinalityDelay = FinalizationDelay()
				data.AverageBalance = string(utils.FormatBalance(uint64(epochs[i].AverageValidatorBalance), currency))
				break
			}
		}

		data.StakedEther = string(utils.FormatBalance(epochs[len(epochs)-1].EligibleEther, currency))
		data.ActiveValidators = epochs[len(epochs)-1].ValidatorsCount
	}

	if data.CurrentFinalizedEpoch == 0 {
		var epochLowerBound uint64
		if epochLowerBound = 0; epoch > 1600 {
			epochLowerBound = epoch - 1600
		}
		var epochHistory []*types.IndexPageEpochHistory
		err = db.WriterDb.Select(&epochHistory, "SELECT epoch, eligibleether, validatorscount, finalized, averagevalidatorbalance FROM epochs WHERE epoch < $1 and epoch > $2 ORDER BY epoch", epoch, epochLowerBound)
		if err != nil {
			return nil, fmt.Errorf("error retrieving staked ether history: %v", err)
		}

		if len(epochHistory) > 0 {
			for i := len(epochHistory) - 1; i >= 0; i-- {
				if epochHistory[i].Finalized {
					data.CurrentFinalizedEpoch = epochHistory[i].Epoch
					data.FinalityDelay = FinalizationDelay()
					data.AverageBalance = string(utils.FormatBalance(uint64(epochHistory[i].AverageValidatorBalance), currency))
					break
				}
			}

			data.StakedEther = string(utils.FormatBalance(epochHistory[len(epochHistory)-1].EligibleEther, currency))
			data.ActiveValidators = epochHistory[len(epochHistory)-1].ValidatorsCount
		}
	}

	epochsMap := make(map[uint64]*types.IndexPageDataEpochs)
	// set epoch struct values and add to map
	for _, epoch := range epochs {
		// since the latest epoch in the db always has a participation rate of 1, check for < 1 instead of <= 1
		if data.EpochParticipationRate == 0 && epoch.GlobalParticipationRate < 1 {
			data.EpochParticipationRate = epoch.GlobalParticipationRate
		}
		epoch.Ts = utils.EpochToTime(epoch.Epoch)
		epoch.FinalizedFormatted = utils.FormatEpochStatus(epoch.Finalized, epoch.GlobalParticipationRate)
		epoch.GlobalParticipationRateFormatted = utils.FormatGlobalParticipationRateStyle(epoch.VotedEther, epoch.GlobalParticipationRate, currency)
		epoch.ExecutionReward = big.NewInt(0)
		epochsMap[epoch.Epoch] = epoch
	}

	// get slots from db
	// it may be possible for multiple slots to have the same block number, which is why more than 10 slots are selected
	var slots []*types.IndexPageDataBlocks
	err = db.ReaderDb.Select(&slots, `
		SELECT
			blocks.epoch,
			blocks.slot,
			blocks.proposer,
			blocks.blockroot,
			blocks.status,
			COALESCE(blocks.exec_block_number, 0) AS exec_block_number,
			COALESCE(validator_names.name, '') AS name
		FROM blocks 
		LEFT JOIN validators ON blocks.proposer = validators.validatorindex
		LEFT JOIN validator_names ON validators.pubkey = validator_names.publickey
		WHERE blocks.slot < $1
		ORDER BY blocks.slot DESC LIMIT 15`, cutoffSlot)
	if err != nil {
		return nil, fmt.Errorf("error retrieving index block data: %v", err)
	}
	// keep slots with higher blockroot length
	slotsMap := make(map[uint64]*types.IndexPageDataBlocks)
	for _, slot := range slots {
		if slotsMap[slot.Slot] == nil || len(slot.BlockRoot) > len(slotsMap[slot.Slot].BlockRoot) {
			slotsMap[slot.Slot] = slot
		}
	}

	// set block struct values
	for _, slot := range slotsMap {
		slot.StatusFormatted = utils.FormatBlockStatusStyle(slot.Status)
		slot.ProposerFormatted = utils.FormatValidatorWithName(slot.Proposer, slot.ProposerName)
		slot.ExecutionRewardFormatted = template.HTML("-")
		slot.ExecutionRewardRecipient = template.HTML("-")
		slot.Ts = utils.SlotToTime(slot.Slot)

		// it may be possible for the slot to be in a epoch not selected in the previous epoch query (e.g. a new epoch started in the meanwhile)
		if _, containsEpoch := epochsMap[slot.Epoch]; !containsEpoch {
			epochsMap[slot.Epoch] = &types.IndexPageDataEpochs{
				Epoch:                            slot.Epoch,
				Ts:                               utils.EpochToTime(slot.Epoch),
				Finalized:                        false,
				FinalizedFormatted:               utils.FormatEpochStatus(false, 0),
				GlobalParticipationRate:          0,
				GlobalParticipationRateFormatted: utils.FormatGlobalParticipationRateStyle(0, 1, ""),
				ExecutionReward:                  big.NewInt(0),
				VotedEther:                       0,
			}
		}
	}

	// get highest and lowest exec block number contained in the slots/epochs shown on this page
	blockNumStats := struct {
		Min uint64 `db:"min"`
		Max uint64 `db:"max"`
	}{}
	err = db.ReaderDb.Get(&blockNumStats, `
		SELECT MIN(b.exec_block_number) as min, MAX(b.exec_block_number) as max
		FROM blocks b
		WHERE b.slot < $1 AND b.epoch >= $2 AND b.exec_block_number != 0`, cutoffSlot, minEpoch)
	if err != nil {
		return nil, fmt.Errorf("error retrieving min and max block num for landing page: %v", err)
	}
	data.CurrentBlock = blockNumStats.Max

	// check if there even are any el blocks
	if blockNumStats.Min != 0 {
		// get el block data from bigtable
		elBlocks, err := db.BigtableClient.GetBlocksDescending(blockNumStats.Max, (blockNumStats.Max-blockNumStats.Min)+1)
		if err != nil {
			return nil, fmt.Errorf("error retrieving el block data for landing page: %v", err)
		}

		// get relay data for all blocks
		relayData, err := db.GetRelayDataForIndexedBlocks(elBlocks)
		if err != nil {
			return nil, fmt.Errorf("error retrieving relay data for blocks for landing page: %v", err)
		}

		// sum up or set reward for epochs and slots respectively
		for _, block := range elBlocks {
			slotNum := utils.TimeToSlot(uint64(block.Time.AsTime().Unix()))
			epochNum := utils.EpochOfSlot(slotNum)
			slot, slotInMap := slotsMap[slotNum]
			if relayDatum, blockHasMEV := relayData[common.BytesToHash(block.Hash)]; blockHasMEV {
				epochsMap[epochNum].ExecutionReward = new(big.Int).Add(epochsMap[epochNum].ExecutionReward, relayDatum.MevBribe.BigInt())
				if slotInMap {
					slot.ExecutionRewardFormatted = utils.NewFormat(relayDatum.MevBribe.BigInt(), "ETH", 5, 2, 1)
					slot.ExecutionRewardRecipient = utils.FormatAddressWithLimits(relayDatum.MevRecipient, "", false, "address", 15, 20, false)
				}
			} else {
				epochsMap[epochNum].ExecutionReward = new(big.Int).Add(epochsMap[epochNum].ExecutionReward, new(big.Int).SetBytes(block.TxReward))
				if slotInMap {
					slot.ExecutionRewardFormatted = utils.NewFormat(new(big.Int).SetBytes(block.TxReward), "ETH", 5, 2, 1)
					slot.ExecutionRewardRecipient = utils.FormatAddressWithLimits(block.Coinbase, "", false, "address", 15, 20, false)
				}
			}
		}
	}

	// convert slotsMap to slice
	slots = make([]*types.IndexPageDataBlocks, 0, len(slots))
	for _, s := range slotsMap {
		slots = append(slots, s)
	}
	sort.Slice(slots, func(i, j int) bool {
		return slots[i].Slot > slots[j].Slot
	})

	if len(slots) > 10 {
		slots = slots[:10]
	}

	data.Blocks = slots

	// convert epochsMap to slice
	epochs = make([]*types.IndexPageDataEpochs, 0, len(epochs))
	for _, e := range epochsMap {
		epochs = append(epochs, e)
	}
	sort.Slice(epochs, func(i, j int) bool {
		return epochs[i].Epoch > epochs[j].Epoch
	})

	if len(epochs) > 10 {
		epochs = epochs[:10]
	}

	for _, e := range epochs {
		e.ExecutionRewardFormatted = utils.NewFormat(e.ExecutionReward, "ETH", 4, 0, 1)
	}
	data.Epochs = epochs

	// not utilized in design prototype
	//data.Subtitle = template.HTML(utils.Config.Frontend.SiteSubtitle)
	data.ChurnRate = *GetLatestStats().ValidatorChurnLimit

	// get eth.store
	err = db.ReaderDb.Get(&data.EthStore, `
		SELECT apr
		FROM eth_store_stats e
		WHERE validator = -1 AND day = (SELECT MAX(day) FROM eth_store_stats);`)

	if err != nil {
		return nil, fmt.Errorf("error retrieving eth store for index page: %v", err)
	}

	// get gas price history
	now = time.Now().Truncate(time.Minute)
	lastThreeDays := time.Now().Truncate(time.Minute).Add(-time.Hour * 24 * 3)

	history, err := db.BigtableClient.GetGasNowHistory(now, lastThreeDays)
	if err != nil {
		return nil, fmt.Errorf("error retrieving gas price history: %v", err)
	}

	group := make(map[int64]float64, 0)
	count := make(map[int64]int, 0)
	for i := 0; i < len(history); i++ {
		ts := history[i].Ts.Truncate(time.Hour).Unix()
		group[ts] += float64(history[i].Fast.Int64())
		count[ts]++
	}
	for ts, sum := range group {
		group[ts] = sum / float64(count[ts])
	}

	gasPriceData := [][]float64{}

	for ts, fast := range group {
		gasPriceData = append(gasPriceData, []float64{float64(ts * 1000), math.Round(fast/1e4) / 1e5})
	}

	sort.SliceStable(gasPriceData, func(i int, j int) bool {
		return gasPriceData[i][0] < gasPriceData[j][0]
	})

	data.GasPriceHistory = gasPriceData

	return data, nil
}

// LatestEpoch will return the latest epoch
func LatestEpoch() uint64 {
	cacheKey := fmt.Sprintf("%d:frontend:latestEpoch", utils.Config.Chain.Config.DepositChainID)

	if wanted, err := cache.TieredCache.GetUint64WithLocalTimeout(cacheKey, time.Second*5); err == nil {
		return wanted
	} else {
		logger.Errorf("error retrieving latestEpoch from cache: %v", err)
	}

	return 0
}

func LatestNodeEpoch() uint64 {
	cacheKey := fmt.Sprintf("%d:frontend:latestNodeEpoch", utils.Config.Chain.Config.DepositChainID)

	if wanted, err := cache.TieredCache.GetUint64WithLocalTimeout(cacheKey, time.Second*5); err == nil {
		return wanted
	} else {
		logger.Errorf("error retrieving latestNodeEpoch from cache: %v", err)
	}

	return 0
}

func LatestNodeFinalizedEpoch() uint64 {
	cacheKey := fmt.Sprintf("%d:frontend:latestNodeFinalizedEpoch", utils.Config.Chain.Config.DepositChainID)

	if wanted, err := cache.TieredCache.GetUint64WithLocalTimeout(cacheKey, time.Second*5); err == nil {
		return wanted
	} else {
		logger.Errorf("error retrieving latestNodeFinalizedEpoch from cache: %v", err)
	}

	return 0
}

// LatestFinalizedEpoch will return the most recent epoch that has been finalized.
func LatestFinalizedEpoch() uint64 {
	cacheKey := fmt.Sprintf("%d:frontend:latestFinalized", utils.Config.Chain.Config.DepositChainID)

	if wanted, err := cache.TieredCache.GetUint64WithLocalTimeout(cacheKey, time.Second*5); err == nil {
		return wanted
	} else {
		logger.Errorf("error retrieving latestFinalized for key: %v from cache: %v", cacheKey, err)
	}
	return 0
}

// LatestSlot will return the latest slot
func LatestSlot() uint64 {
	cacheKey := fmt.Sprintf("%d:frontend:slot", utils.Config.Chain.Config.DepositChainID)

	if wanted, err := cache.TieredCache.GetUint64WithLocalTimeout(cacheKey, time.Second*5); err == nil {
		return wanted
	} else {
		logger.Errorf("error retrieving latest slot from cache: %v", err)
	}
	return 0
}

// FinalizationDelay will return the current Finalization Delay
func FinalizationDelay() uint64 {
	return LatestNodeEpoch() - LatestNodeFinalizedEpoch()
}

// LatestProposedSlot will return the latest proposed slot
func LatestProposedSlot() uint64 {
	cacheKey := fmt.Sprintf("%d:frontend:latestProposedSlot", utils.Config.Chain.Config.DepositChainID)

	if wanted, err := cache.TieredCache.GetUint64WithLocalTimeout(cacheKey, time.Second*5); err == nil {
		return wanted
	} else {
		logger.Errorf("error retrieving latestProposedSlot from cache: %v", err)
	}
	return 0
}

func LatestMempoolTransactions() *types.RawMempoolResponse {
	wanted := &types.RawMempoolResponse{}
	cacheKey := fmt.Sprintf("%d:frontend:mempool", utils.Config.Chain.Config.DepositChainID)
	if wanted, err := cache.TieredCache.GetWithLocalTimeout(cacheKey, time.Second*60, wanted); err == nil {
		return wanted.(*types.RawMempoolResponse)
	} else {
		logger.Errorf("error retrieving mempool data from cache: %v", err)
	}
	return &types.RawMempoolResponse{}
}

func LatestBurnData() *types.BurnPageData {
	wanted := &types.BurnPageData{}
	cacheKey := fmt.Sprintf("%d:frontend:burn", utils.Config.Chain.Config.DepositChainID)
	if wanted, err := cache.TieredCache.GetWithLocalTimeout(cacheKey, time.Second*60, wanted); err == nil {
		return wanted.(*types.BurnPageData)
	} else {
		logger.Errorf("error retrieving burn data from cache: %v", err)
	}
	return &types.BurnPageData{}
}

// LatestIndexPageData returns the latest index page data
func LatestIndexPageData() *types.IndexPageData {
	wanted := &types.IndexPageData{}
	cacheKey := fmt.Sprintf("%d:frontend:indexPageData", utils.Config.Chain.Config.DepositChainID)

	if wanted, err := cache.TieredCache.GetWithLocalTimeout(cacheKey, time.Second*5, wanted); err == nil {
		return wanted.(*types.IndexPageData)
	} else {
		logger.Errorf("error retrieving indexPageData from cache: %v", err)
	}
	return &types.IndexPageData{
		NetworkName:           "",
		DepositContract:       "",
		ShowSyncingMessage:    false,
		CurrentEpoch:          0,
		CurrentFinalizedEpoch: 0,
		CurrentBlock:          0,
		ScheduledCount:        0,
		FinalityDelay:         0,
		ActiveValidators:      0,
		EnteringValidators:    0,
		ExitingValidators:     0,
		StakedEther:           "",
		AverageBalance:        "",
		DepositedTotal:        0,
		DepositThreshold:      0,
		ValidatorsRemaining:   0,
		NetworkStartTs:        0,
		MinGenesisTime:        0,
		Blocks:                []*types.IndexPageDataBlocks{},
		Epochs:                []*types.IndexPageDataEpochs{},
		//StakedEtherChartData:      [][]float64{},
		//ActiveValidatorsChartData: [][]float64{},
		Subtitle:            "",
		Genesis:             false,
		GenesisPeriod:       false,
		Mainnet:             false,
		DepositChart:        &types.ChartsPageDataChart{},
		DepositDistribution: &types.ChartsPageDataChart{},
		Countdown:           nil,
		SlotVizData:         types.SlotVizPageData{},
	}
}

// LatestPoolsPageData returns the latest pools page data
func LatestPoolsPageData() *types.PoolsResp {

	wanted := &types.PoolsResp{}
	cacheKey := fmt.Sprintf("%d:frontend:poolsData", utils.Config.Chain.Config.DepositChainID)

	if wanted, err := cache.TieredCache.GetWithLocalTimeout(cacheKey, time.Second*5, wanted); err == nil {
		return wanted.(*types.PoolsResp)
	} else {
		logger.Errorf("error retrieving poolsData from cache: %v", err)
	}

	return &types.PoolsResp{
		PoolsDistribution:       types.ChartsPageDataChart{},
		HistoricPoolPerformance: types.ChartsPageDataChart{},
		PoolInfos:               []*types.PoolInfo{},
	}
}

func LatestGasNowData() *types.GasNowPageData {
	wanted := &types.GasNowPageData{}
	cacheKey := fmt.Sprintf("%d:frontend:gasNow", utils.Config.Chain.Config.DepositChainID)

	if wanted, err := cache.TieredCache.GetWithLocalTimeout(cacheKey, time.Second*5, wanted); err == nil {
		return wanted.(*types.GasNowPageData)
	} else {
		logger.Errorf("error retrieving gasNow from cache: %v", err)
	}

	return nil
}

func LatestRelaysPageData() *types.RelaysResp {
	wanted := &types.RelaysResp{}
	cacheKey := fmt.Sprintf("%d:frontend:relaysData", utils.Config.Chain.Config.DepositChainID)

	if wanted, err := cache.TieredCache.GetWithLocalTimeout(cacheKey, time.Second*5, wanted); err == nil {
		return wanted.(*types.RelaysResp)
	} else {
		logger.Errorf("error retrieving relaysData from cache: %v", err)
	}

	return nil
}

func LatestSlotVizMetrics() []*types.SlotVizEpochs {
	wanted := &[]*types.SlotVizEpochs{}
	cacheKey := fmt.Sprintf("%d:frontend:slotVizMetrics", utils.Config.Chain.Config.DepositChainID)

	if wanted, err := cache.TieredCache.GetWithLocalTimeout(cacheKey, time.Second*5, wanted); err == nil {
		w := wanted.(*[]*types.SlotVizEpochs)
		return *w
	} else {
		logger.Errorf("error retrieving slotVizMetrics from cache: %v", err)
	}

	return []*types.SlotVizEpochs{}
}

// LatestState returns statistics about the current eth2 state
func LatestState() *types.LatestState {
	data := &types.LatestState{}
	data.CurrentEpoch = LatestEpoch()
	data.CurrentSlot = LatestSlot()
	data.CurrentFinalizedEpoch = LatestFinalizedEpoch()
	data.LastProposedSlot = LatestProposedSlot()
	data.FinalityDelay = FinalizationDelay()
	data.IsSyncing = IsSyncing()
	data.UsdRoundPrice = price.GetEthRoundPrice(price.GetEthPrice("USD"))
	data.UsdTruncPrice = utils.KFormatterEthPrice(data.UsdRoundPrice)
	data.EurRoundPrice = price.GetEthRoundPrice(price.GetEthPrice("EUR"))
	data.EurTruncPrice = utils.KFormatterEthPrice(data.EurRoundPrice)
	data.GbpRoundPrice = price.GetEthRoundPrice(price.GetEthPrice("GBP"))
	data.GbpTruncPrice = utils.KFormatterEthPrice(data.GbpRoundPrice)
	data.CnyRoundPrice = price.GetEthRoundPrice(price.GetEthPrice("CNY"))
	data.CnyTruncPrice = utils.KFormatterEthPrice(data.CnyRoundPrice)
	data.RubRoundPrice = price.GetEthRoundPrice(price.GetEthPrice("RUB"))
	data.RubTruncPrice = utils.KFormatterEthPrice(data.RubRoundPrice)
	data.CadRoundPrice = price.GetEthRoundPrice(price.GetEthPrice("CAD"))
	data.CadTruncPrice = utils.KFormatterEthPrice(data.CadRoundPrice)
	data.AudRoundPrice = price.GetEthRoundPrice(price.GetEthPrice("AUD"))
	data.AudTruncPrice = utils.KFormatterEthPrice(data.AudRoundPrice)
	data.JpyRoundPrice = price.GetEthRoundPrice(price.GetEthPrice("JPY"))
	data.JpyTruncPrice = utils.KFormatterEthPrice(data.JpyRoundPrice)

	return data
}

func GetLatestStats() *types.Stats {
	wanted := &types.Stats{}
	cacheKey := fmt.Sprintf("%d:frontend:latestStats", utils.Config.Chain.Config.DepositChainID)

	if wanted, err := cache.TieredCache.GetWithLocalTimeout(cacheKey, time.Second*5, wanted); err == nil {
		return wanted.(*types.Stats)
	} else {
		logger.Errorf("error retrieving slotVizMetrics from cache: %v", err)
	}

	// create an empty stats object if no stats exist (genesis)
	return &types.Stats{
		TopDepositors: &[]types.StatsTopDepositors{
			{
				Address:      "000",
				DepositCount: 0,
			},
			{
				Address:      "000",
				DepositCount: 0,
			},
		},
		InvalidDepositCount:   new(uint64),
		UniqueValidatorCount:  new(uint64),
		TotalValidatorCount:   new(uint64),
		ActiveValidatorCount:  new(uint64),
		PendingValidatorCount: new(uint64),
		ValidatorChurnLimit:   new(uint64),
	}
}

// IsSyncing returns true if the chain is still syncing
func IsSyncing() bool {
	return time.Now().Add(time.Minute * -10).After(utils.EpochToTime(LatestEpoch()))
}

func gasNowUpdater(wg *sync.WaitGroup) {
	firstRun := true

	for {
		data, err := getGasNowData()
		if err != nil {
			logger.Errorf("error retrieving gas now data: %v", err)
			time.Sleep(time.Second * 5)
			continue
		}

		cacheKey := fmt.Sprintf("%d:frontend:gasNow", utils.Config.Chain.Config.DepositChainID)
		err = cache.TieredCache.Set(cacheKey, data, time.Hour*24)
		if err != nil {
			logger.Errorf("error caching latestFinalizedEpoch: %v", err)
		}
		if firstRun {
			wg.Done()
			firstRun = false
		}
		time.Sleep(time.Second * 5)
	}
}

func getGasNowData() (*types.GasNowPageData, error) {
	gpoData := &types.GasNowPageData{}
	gpoData.Code = 200
	gpoData.Data.Timestamp = time.Now().UnixNano() / 1e6

	client, err := geth_rpc.Dial(utils.Config.Eth1GethEndpoint)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage

	err = client.Call(&raw, "eth_getBlockByNumber", "pending", true)
	if err != nil {
		return nil, fmt.Errorf("error retrieving pending block data: %v", err)
	}

	var header *geth_types.Header
	var body rpcBlock

	err = json.Unmarshal(raw, &header)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(raw, &body)
	if err != nil {
		return nil, err
	}
	txs := body.Transactions

	sort.Slice(txs, func(i, j int) bool {
		return txs[i].tx.GasPrice().Cmp(txs[j].tx.GasPrice()) > 0
	})
	if len(txs) > 1 {
		medianGasPrice := txs[len(txs)/2].tx.GasPrice()
		tailGasPrice := txs[len(txs)-1].tx.GasPrice()

		gpoData.Data.Rapid = medianGasPrice
		gpoData.Data.Fast = tailGasPrice
	} else {
		return nil, fmt.Errorf("current pending block contains no tx")
	}

	err = client.Call(&raw, "txpool_content")
	if err != nil {
		logrus.Fatal(err)
	}

	txPoolContent := &TxPoolContent{}
	err = json.Unmarshal(raw, txPoolContent)
	if err != nil {
		logrus.Fatal(err)
	}

	pendingTxs := make([]*geth_types.Transaction, 0, len(txPoolContent.Pending))

	for _, account := range txPoolContent.Pending {
		lowestNonce := 9223372036854775807
		for n := range account {
			if n < int(lowestNonce) {
				lowestNonce = n
			}
		}

		pendingTxs = append(pendingTxs, account[lowestNonce])
	}
	sort.Slice(pendingTxs, func(i, j int) bool {
		return pendingTxs[i].GasPrice().Cmp(pendingTxs[j].GasPrice()) > 0
	})

	standardIndex := int(math.Max(float64(2*len(txs)), 500))

	slowIndex := int(math.Max(float64(5*len(txs)), 1000))
	if standardIndex < len(pendingTxs) {
		gpoData.Data.Standard = pendingTxs[standardIndex].GasPrice()
	} else {
		gpoData.Data.Standard = header.BaseFee
	}

	if slowIndex < len(pendingTxs) {
		gpoData.Data.Slow = pendingTxs[slowIndex].GasPrice()
	} else {
		gpoData.Data.Slow = header.BaseFee
	}

	err = db.BigtableClient.SaveGasNowHistory(gpoData.Data.Slow, gpoData.Data.Standard, gpoData.Data.Fast, gpoData.Data.Rapid)
	if err != nil {
		logrus.WithError(err).Error("error updating gas now history")
	}

	gpoData.Data.Price = price.GetEthPrice("USD")
	gpoData.Data.Currency = "USD"

	// gpoData.RapidUSD = gpoData.Rapid * 21000 * params.GWei / params.Ether * usd
	// gpoData.FastUSD = gpoData.Fast * 21000 * params.GWei / params.Ether * usd
	// gpoData.StandardUSD = gpoData.Standard * 21000 * params.GWei / params.Ether * usd
	// gpoData.SlowUSD = gpoData.Slow * 21000 * params.GWei / params.Ether * usd
	return gpoData, nil
}

type TxPoolContent struct {
	Pending map[string]map[int]*geth_types.Transaction
}

type rpcTransaction struct {
	tx *geth_types.Transaction
	txExtraInfo
}

type txExtraInfo struct {
	BlockNumber *string         `json:"blockNumber,omitempty"`
	BlockHash   *common.Hash    `json:"blockHash,omitempty"`
	From        *common.Address `json:"from,omitempty"`
}

type rpcBlock struct {
	Transactions []rpcTransaction `json:"transactions"`
}

func (tx *rpcTransaction) UnmarshalJSON(msg []byte) error {
	if err := json.Unmarshal(msg, &tx.tx); err != nil {
		return err
	}
	return json.Unmarshal(msg, &tx.txExtraInfo)
}

func mempoolUpdater(wg *sync.WaitGroup) {
	firstRun := true
	for {
		client, err := geth_rpc.Dial(utils.Config.Eth1GethEndpoint)
		if err != nil {
			logrus.Error("Can't connect to geth node: ", err)
			time.Sleep(time.Second * 30)
			continue
		}

		var mempoolTx types.RawMempoolResponse

		err = client.Call(&mempoolTx, "txpool_content")
		if err != nil {
			logrus.Error("Error calling txpool_content request: ", err)
			time.Sleep(time.Second * 10)
			continue
		}

		cacheKey := fmt.Sprintf("%d:frontend:mempool", utils.Config.Chain.Config.DepositChainID)
		err = cache.TieredCache.Set(cacheKey, mempoolTx, time.Hour*24)
		if err != nil {
			logger.Errorf("error caching relaysData: %v", err)
		}
		if firstRun {
			logger.Info("initialized mempool updater")
			wg.Done()
			firstRun = false
		}
		ReportStatus("mempoolUpdater", "Running", nil)
		time.Sleep(time.Second * 1)
	}
}

func burnUpdater(wg *sync.WaitGroup) {
	firstRun := true
	for {
		data, err := getBurnPageData()
		if err != nil {
			logger.Errorf("error retrieving burn page data: %v", err)
			time.Sleep(time.Second * 30)
			continue
		}
		cacheKey := fmt.Sprintf("%d:frontend:burn", utils.Config.Chain.Config.DepositChainID)
		err = cache.TieredCache.Set(cacheKey, data, time.Hour*24)
		if err != nil {
			logger.Errorf("error caching relaysData: %v", err)
		}
		if firstRun {
			logger.Infof("initialized burn updater")
			wg.Done()
			firstRun = false
		}
		time.Sleep(time.Second * 30)
	}
}

func getBurnPageData() (*types.BurnPageData, error) {
	data := &types.BurnPageData{}
	start := time.Now()

	latestEpoch := LatestEpoch()
	latestBlock := LatestEth1BlockNumber()

	// Retrieve the total amount of burned Ether
	err := db.ReaderDb.Get(&data.TotalBurned, "select sum(value) from chart_series where indicator = 'BURNED_FEES';")
	if err != nil {
		return nil, fmt.Errorf("error retrieving total burned amount from chart_series table: %v", err)
	}

	cutOff := time.Time{}
	err = db.ReaderDb.Get(&cutOff, "select (select max(time) from chart_series where indicator = 'BURNED_FEES') + interval '24 hours';")
	if err != nil {
		return nil, fmt.Errorf("error retrieving cutoff date from chart_series table: %v", err)
	}

	cutOffEpoch := utils.TimeToEpoch(cutOff)
	// logger.Infof("cutoff epoch: %v", cutOffEpoch)
	// var blockLastHour uint64
	// db.ReaderDb.Get(&blockLastHour, "select ")

	// var blockLastDay uint64
	// db.ReaderDb.Get(&blockLastDay)

	additionalBurned := float64(0)
	err = db.ReaderDb.Get(&additionalBurned, "select COALESCE(SUM(exec_base_fee_per_gas::numeric * exec_gas_used::numeric), 0) as burnedfees from blocks where epoch > $1", cutOffEpoch)
	if err != nil {
		return nil, fmt.Errorf("error retrieving additional burned eth from blocks table: %v", err)
	}
	// logger.Infof("additonal burn: %v", additionalBurned)
	data.TotalBurned += additionalBurned

	err = db.ReaderDb.Get(&data.BurnRate1h, "select COALESCE(SUM(exec_base_fee_per_gas::numeric * exec_gas_used::numeric) / 60, 0) as burnedfees from blocks where epoch > $1", latestEpoch-10)
	if err != nil {
		return nil, fmt.Errorf("error retrieving burn rate (1h) from blocks table: %v", err)
	}

	// err = db.ReaderDb.Get(&data.Emission, "select total_rewards_wei as emission from eth_store_stats order by day desc limit 1")
	// if err != nil {
	// 	return nil, fmt.Errorf("error retrieving emission (24h): %v", err)
	// }

	// swap this for GetEpochIncomeHistory in the future
	income, err := db.BigtableClient.GetValidatorIncomeDetailsHistory([]uint64{}, latestEpoch, 10)
	if err != nil {
		logger.WithError(err).Error("error getting validator income history")
	}

	total := &itypes.ValidatorEpochIncome{}

	for _, epochs := range income {
		// logger.Infof("epochs: %+v", epochs)
		for _, details := range epochs {
			// logger.Infof("income: %+v", details)
			total.AttestationHeadReward += details.AttestationHeadReward
			total.AttestationSourceReward += details.AttestationSourceReward
			total.AttestationSourcePenalty += details.AttestationSourcePenalty
			total.AttestationTargetReward += details.AttestationTargetReward
			total.AttestationTargetPenalty += details.AttestationTargetPenalty
			total.FinalityDelayPenalty += details.FinalityDelayPenalty
			total.ProposerSlashingInclusionReward += details.ProposerSlashingInclusionReward
			total.ProposerAttestationInclusionReward += details.ProposerAttestationInclusionReward
			total.ProposerSyncInclusionReward += details.ProposerSyncInclusionReward
			total.SyncCommitteeReward += details.SyncCommitteeReward
			total.SyncCommitteePenalty += details.SyncCommitteePenalty
			total.SlashingReward += details.SlashingReward
			total.SlashingPenalty += details.SlashingPenalty
			total.TxFeeRewardWei = utils.AddBigInts(total.TxFeeRewardWei, details.TxFeeRewardWei)
		}
	}

	rewards := decimal.NewFromBigInt(new(big.Int).SetBytes(total.TxFeeRewardWei), 0)

	rewards = rewards.Add(decimal.NewFromBigInt(new(big.Int).SetUint64(total.AttestationHeadReward), 0))
	rewards = rewards.Add(decimal.NewFromBigInt(new(big.Int).SetUint64(total.AttestationSourceReward), 0))
	rewards = rewards.Add(decimal.NewFromBigInt(new(big.Int).SetUint64(total.AttestationTargetReward), 0))
	rewards = rewards.Add(decimal.NewFromBigInt(new(big.Int).SetUint64(total.ProposerSlashingInclusionReward), 0))
	rewards = rewards.Add(decimal.NewFromBigInt(new(big.Int).SetUint64(total.ProposerAttestationInclusionReward), 0))
	rewards = rewards.Add(decimal.NewFromBigInt(new(big.Int).SetUint64(total.ProposerSyncInclusionReward), 0))
	rewards = rewards.Add(decimal.NewFromBigInt(new(big.Int).SetUint64(total.SyncCommitteeReward), 0))
	rewards = rewards.Add(decimal.NewFromBigInt(new(big.Int).SetUint64(total.SlashingReward), 0))

	rewards = rewards.Sub(decimal.NewFromBigInt(new(big.Int).SetUint64(total.AttestationTargetPenalty), 0))
	rewards = rewards.Sub(decimal.NewFromBigInt(new(big.Int).SetUint64(total.FinalityDelayPenalty), 0))
	rewards = rewards.Sub(decimal.NewFromBigInt(new(big.Int).SetUint64(total.SyncCommitteePenalty), 0))
	rewards = rewards.Sub(decimal.NewFromBigInt(new(big.Int).SetUint64(total.AttestationSourcePenalty), 0))
	rewards = rewards.Sub(decimal.NewFromBigInt(new(big.Int).SetUint64(total.SlashingPenalty), 0))

	// rewards per min
	rewards = rewards.Div(decimal.NewFromInt(64))

	// emission per minute
	data.Emission = rewards.InexactFloat64() - data.BurnRate1h

	logger.Infof("burn rate per min: %v inflation per min: %v emission: %v", data.BurnRate1h, rewards.InexactFloat64(), data.Emission)
	// logger.Infof("calculated emission: %v", data.Emission)

	err = db.ReaderDb.Get(&data.BurnRate24h, "select COALESCE(SUM(exec_base_fee_per_gas::numeric * exec_gas_used::numeric) / (60 * 24), 0) as burnedfees from blocks where epoch >= $1", latestEpoch-225)
	if err != nil {
		return nil, fmt.Errorf("error retrieving burn rate (24h) from blocks table: %v", err)
	}

	err = db.ReaderDb.Get(&data.BlockUtilization, "select avg(exec_gas_used::numeric * 100 / exec_gas_limit) from blocks where epoch >= $1 and exec_gas_used > 0 and exec_gas_limit > 0", latestEpoch-225)
	if err != nil {
		return nil, fmt.Errorf("error retrieving block utilization from blocks table: %v", err)
	}

	blocks, err := db.BigtableClient.GetBlocksDescending(latestBlock, 1000)
	if err != nil {
		return nil, err
	}

	// db.BigAdminClient

	data.Blocks = make([]*types.BurnPageDataBlock, 0, 1000)
	for _, blk := range blocks {

		blockNumber := blk.GetNumber()
		baseFee := new(big.Int).SetBytes(blk.GetBaseFee())
		// gasHalf := float64(blk.GetGasLimit()) / 2.0
		txReward := new(big.Int).SetBytes(blk.GetTxReward())

		burned := new(big.Int).Mul(baseFee, big.NewInt(int64(blk.GetGasUsed())))
		// burnedPercentage := float64(0.0)
		if len(txReward.Bits()) != 0 {
			txBurnedBig := new(big.Float).SetInt(burned)
			txBurnedBig.Quo(txBurnedBig, new(big.Float).SetInt(txReward))
			// burnedPercentage, _ = txBurnedBig.Float64()
		}

		blockReward := new(big.Int).Add(utils.Eth1BlockReward(blockNumber, blk.GetDifficulty()), new(big.Int).Add(txReward, new(big.Int).SetBytes(blk.GetUncleReward())))

		data.Blocks = append(data.Blocks, &types.BurnPageDataBlock{
			Number:        int64(blockNumber),
			Hash:          hex.EncodeToString(blk.Hash),
			GasTarget:     int64(blk.GasLimit),
			GasUsed:       int64(blk.GasUsed),
			Txn:           int(blk.TransactionCount),
			Age:           blk.Time.AsTime(),
			BaseFeePerGas: float64(baseFee.Int64()),
			BurnedFees:    float64(burned.Int64()),
			Rewards:       float64(blockReward.Int64()),
		})
	}

	if len(data.Blocks) > 100 {
		if data.Blocks[0].BaseFeePerGas < data.Blocks[100].BaseFeePerGas {
			data.BaseFeeTrend = -1
		} else if data.Blocks[0].BaseFeePerGas == data.Blocks[100].BaseFeePerGas {
			data.BaseFeeTrend = 0
		} else {
			data.BaseFeeTrend = 1
		}
	} else {
		data.BaseFeeTrend = 1
	}

	for _, b := range data.Blocks {
		if b.Number > 12965000 {
			b.GasTarget = b.GasTarget / 2
		}
	}
	logger.Infof("epoch burn page export took: %v seconds", time.Since(start).Seconds())
	return data, nil
}
