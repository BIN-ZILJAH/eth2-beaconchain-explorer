package main

import (
	"eth2-exporter/db"
	"eth2-exporter/rpc"
	"eth2-exporter/types"
	"flag"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v4/stdlib"
	"github.com/karlseguin/ccache/v2"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

func main() {
	// localhost:8545
	erigonEndpoint := flag.String("erigon", "", "Erigon archive node enpoint")

	block := flag.Int64("block", 0, "Index a specific block")

	concurrencyBlocks := flag.Int64("blocks.concurrency", 30, "Concurrency to use when indexing blocks from erigon")
	startBlocks := flag.Int64("blocks.start", 0, "Block to start indexing")
	endBlocks := flag.Int64("blocks.end", 0, "Block to finish indexing")
	offsetBlocks := flag.Int64("blocks.offset", 100, "Blocks offset")
	checkBlocksGaps := flag.Bool("blocks.gaps", false, "Check for gaps in the blocks table")
	checkBlocksGapsLookback := flag.Int("blocks.gaps.lookback", 1000000, "Lookback for gaps check of the blocks table")

	concurrencyData := flag.Int64("data.concurrency", 30, "Concurrency to use when indexing data from bigtable")
	startData := flag.Int64("data.start", 0, "Block to start indexing")
	endData := flag.Int64("data.end", 0, "Block to finish indexing")
	offsetData := flag.Int64("data.offset", 1000, "Data offset")
	checkDataGaps := flag.Bool("data.gaps", false, "Check for gaps in the data table")
	checkDataGapsLookback := flag.Int("data.gaps.lookback", 1000000, "Lookback for gaps check of the blocks table")

	flag.Parse()

	if erigonEndpoint == nil || *erigonEndpoint == "" {
		logrus.Fatal("no erigon node url provided")
	}

	logrus.Infof("using erigon node at %v", *erigonEndpoint)
	client, err := rpc.NewErigonClient(*erigonEndpoint)
	if err != nil {
		logrus.Fatal(err)
	}

	bt, err := db.NewBigtable("etherchain", "etherchain", "1")
	if err != nil {
		logrus.Fatalf("error connecting to bigtable: %v", err)
	}
	defer bt.Close()

	updates, err := bt.GetMetadataUpdates("B:", 10000)
	if err != nil {
		logrus.Fatal(err)
	}

	for _, update := range updates {
		logrus.Infof("updating balance of key %v", update)
		s := strings.Split(update, ":")

		if len(s) != 3 {
			logrus.Fatalf("%v has an invalid format", update)
		}

		if s[0] != "B" {
			logrus.Fatalf("%v has invalid balance update prefix", update)
		}

		address := s[1]
		token := s[2]

		if token == "00" {
			logrus.Infof("updating native balance of address %v", address)
		}
	}
	return

	transforms := make([]func(blk *types.Eth1Block, cache *ccache.Cache) (*types.BulkMutations, *types.BulkMutations, error), 0)
	transforms = append(transforms, bt.TransformBlock, bt.TransformTx, bt.TransformItx, bt.TransformERC20, bt.TransformERC721, bt.TransformERC1155, bt.TransformUncle)

	if *block != 0 {
		err = IndexFromNode(bt, client, *block, *block, *concurrencyBlocks)
		if err != nil {
			logrus.WithError(err).Fatalf("error indexing from node")
		}
		err = IndexFromBigtable(bt, *block, *block, transforms, *concurrencyData)
		if err != nil {
			logrus.WithError(err).Fatalf("error indexing from bigtable")
		}

		logrus.Infof("indexing of block %v completed", *block)
		return
	}

	if *checkBlocksGaps {
		bt.CheckForGapsInBlocksTable(*checkBlocksGapsLookback)
		return
	}

	if *checkDataGaps {
		bt.CheckForGapsInDataTable(*checkDataGapsLookback)
		return
	}

	if *endBlocks != 0 && *startBlocks < *endBlocks {
		err = IndexFromNode(bt, client, *startBlocks, *endBlocks, *concurrencyBlocks)
		if err != nil {
			logrus.WithError(err).Fatalf("error indexing from node")
		}
		return
	}

	if *endData != 0 && *startData < *endData {
		err = IndexFromBigtable(bt, int64(*startData), int64(*endData), transforms, *concurrencyData)
		if err != nil {
			logrus.WithError(err).Fatalf("error indexing from bigtable")
		}
		return
	}

	// return
	// bt.DeleteRowsWithPrefix("1:b:")
	// return

	for {
		lastBlockFromNode, err := client.GetLatestEth1BlockNumber()
		if err != nil {
			logrus.Fatal(err)
		}
		lastBlockFromNode = lastBlockFromNode - 100

		lastBlockFromBlocksTable, err := bt.GetLastBlockInBlocksTable()
		if err != nil {
			logrus.Fatal(err)
		}

		lastBlockFromDataTable, err := bt.GetLastBlockInDataTable()
		if err != nil {
			logrus.Fatal(err)
		}

		logrus.WithFields(
			logrus.Fields{
				"node":   lastBlockFromNode,
				"blocks": lastBlockFromBlocksTable,
				"data":   lastBlockFromDataTable,
			},
		).Infof("last blocks")

		if lastBlockFromBlocksTable < int(lastBlockFromNode) {
			logrus.Infof("missing blocks %v to %v in blocks table, indexing ...", lastBlockFromBlocksTable, lastBlockFromNode)

			err = IndexFromNode(bt, client, int64(lastBlockFromBlocksTable)-*offsetBlocks, int64(lastBlockFromNode), *concurrencyBlocks)
			if err != nil {
				logrus.WithError(err).Fatalf("error indexing from node")
			}
		}

		if lastBlockFromDataTable < int(lastBlockFromNode) {
			// transforms = append(transforms, bt.TransformTx)

			logrus.Infof("missing blocks %v to %v in data table, indexing ...", lastBlockFromDataTable, lastBlockFromNode)
			err = IndexFromBigtable(bt, int64(lastBlockFromDataTable)-*offsetData, int64(lastBlockFromNode), transforms, *concurrencyData)
			if err != nil {
				logrus.WithError(err).Fatalf("error indexing from bigtable")
			}
		}

		logrus.Infof("index run completed")
		time.Sleep(time.Second * 14)
	}

	// utils.WaitForCtrlC()

}

func IndexFromNode(bt *db.Bigtable, client *rpc.ErigonClient, start, end, concurrency int64) error {

	g := new(errgroup.Group)
	g.SetLimit(int(concurrency))

	startTs := time.Now()
	lastTickTs := time.Now()

	processedBlocks := int64(0)

	for i := start; i <= end; i++ {

		i := i
		g.Go(func() error {
			blockStartTs := time.Now()
			bc, timings, err := client.GetBlock(i)

			if err != nil {
				logrus.Error(err)
				return err
			}

			dbStart := time.Now()
			err = bt.SaveBlock(bc)
			if err != nil {
				logrus.Error(err)
				return err
			}
			current := atomic.AddInt64(&processedBlocks, 1)
			if current%100 == 0 {
				r := end - start
				if r == 0 {
					r = 1
				}
				perc := float64(i-start) * 100 / float64(r)

				logrus.Infof("retrieved & saved block %v (0x%x) in %v (header: %v, receipts: %v, traces: %v, db: %v)", bc.Number, bc.Hash, time.Since(blockStartTs), timings.Headers, timings.Receipts, timings.Traces, time.Since(dbStart))
				logrus.Infof("processed %v blocks in %v (%.1f blocks / sec); sync is %.1f%% complete", current, time.Since(startTs), float64((current))/time.Since(lastTickTs).Seconds(), perc)

				lastTickTs = time.Now()
				atomic.StoreInt64(&processedBlocks, 0)
			}
			return nil
		})

	}

	return g.Wait()
}

func IndexFromBigtable(bt *db.Bigtable, start, end int64, transforms []func(blk *types.Eth1Block, cache *ccache.Cache) (bulkData *types.BulkMutations, bulkMetadataUpdates *types.BulkMutations, err error), concurrency int64) error {
	g := new(errgroup.Group)
	g.SetLimit(int(concurrency))

	startTs := time.Now()
	lastTickTs := time.Now()

	processedBlocks := int64(0)

	cache := ccache.New(ccache.Configure().MaxSize(1000000).ItemsToPrune(500))

	logrus.Infof("fetching blocks from %d to %d", start, end)
	for i := start; i <= end; i++ {
		i := i
		g.Go(func() error {

			block, err := bt.GetBlockFromBlocksTable(uint64(i))
			if err != nil {
				logrus.Fatal(err)
				return err
			}

			bulkMutsData := types.BulkMutations{}
			bulkMutsMetadataUpdate := types.BulkMutations{}
			for _, transform := range transforms {
				mutsData, mutsMetadataUpdate, err := transform(block, cache)
				if err != nil {
					logrus.WithError(err).Error("error transforming block")
				}
				bulkMutsData.Keys = append(bulkMutsData.Keys, mutsData.Keys...)
				bulkMutsData.Muts = append(bulkMutsData.Muts, mutsData.Muts...)

				if mutsMetadataUpdate != nil {
					bulkMutsMetadataUpdate.Keys = append(bulkMutsMetadataUpdate.Keys, mutsMetadataUpdate.Keys...)
					bulkMutsMetadataUpdate.Muts = append(bulkMutsMetadataUpdate.Muts, mutsMetadataUpdate.Muts...)
				}
			}

			if len(bulkMutsData.Keys) > 0 {
				err = bt.WriteBulk(&bulkMutsData, bt.GetDataTable())
				if err != nil {
					return fmt.Errorf("error writing to bigtable data table: %w", err)
				}
			}

			if len(bulkMutsMetadataUpdate.Keys) > 0 {
				err = bt.WriteBulk(&bulkMutsMetadataUpdate, bt.GetMetadataUpdatesTable())
				if err != nil {
					return fmt.Errorf("error writing to bigtable metadata updates table: %w", err)
				}
			}

			current := atomic.AddInt64(&processedBlocks, 1)
			if current%500 == 0 {
				r := end - start
				if r == 0 {
					r = 1
				}
				perc := float64(i-start) * 100 / float64(r)
				logrus.Infof("currently processing block: %v; processed %v blocks in %v (%.1f blocks / sec); sync is %.1f%% complete", block.GetNumber(), current, time.Since(startTs), float64((current))/time.Since(lastTickTs).Seconds(), perc)
				lastTickTs = time.Now()
				atomic.StoreInt64(&processedBlocks, 0)
			}
			return nil
		})

	}

	if err := g.Wait(); err == nil {
		logrus.Info("Successfully fetched all blocks")
	} else {
		logrus.Error(err)
		return err
	}

	return nil
}