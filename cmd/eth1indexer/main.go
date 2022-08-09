package main

import (
	"eth2-exporter/db"
	"eth2-exporter/rpc"
	"flag"
	"sync/atomic"
	"time"

	"github.com/davecgh/go-spew/spew"
	_ "github.com/jackc/pgx/v4/stdlib"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

func main() {

	erigonEndpoint := flag.String("erigon", "http://localhost:8545", "Erigon archive node enpoint")
	start := flag.Int64("start", 0, "Block to start indexing")
	end := flag.Int64("end", 0, "Block to finish indexing")

	flag.Parse()

	bt, err := db.NewBigtable("etherchain", "etherchain", "1")
	if err != nil {
		logrus.Fatalf("error connecting to bigtable: %v", err)
	}
	defer bt.Close()

	lastBlock, err := bt.GetLastBlock()

	if err != nil {
		logrus.Fatalf("error retrieving last block from the db: %v", err)
	}
	logrus.Infof("retrieved last block with number %v and hash %x", lastBlock.Number, lastBlock.Hash)
	spew.Dump(lastBlock)
	return

	client, err := rpc.NewErigonClient(*erigonEndpoint)

	if err != nil {
		logrus.Fatal(err)
	}

	g := new(errgroup.Group)
	g.SetLimit(20)

	startTs := time.Now()
	lastTickTs := time.Now()

	processedBlocks := int64(0)

	for i := *end; i >= *start; i-- {

		i := i
		g.Go(func() error {
			blockStartTs := time.Now()
			bc, timings, err := client.GetBlock(i)

			if err != nil {
				return err
			}

			dbStart := time.Now()
			err = bt.SaveBlock(bc)
			if err != nil {
				return err
			}
			current := atomic.AddInt64(&processedBlocks, 1)
			if current%100 == 0 {
				logrus.Infof("retrieved & saved block %v (0x%x) in %v (header: %v, receipts: %v, traces: %v, db: %v)", bc.Number, bc.Hash, time.Since(blockStartTs), timings.Headers, timings.Receipts, timings.Traces, time.Since(dbStart))
				logrus.Infof("processed %v blocks in %v (%.1f blocks / sec)", current, time.Since(startTs), float64((current))/time.Since(lastTickTs).Seconds())

				lastTickTs = time.Now()
				atomic.StoreInt64(&processedBlocks, 0)
			}
			return nil
		})

	}

	if err := g.Wait(); err == nil {
		logrus.Info("Successfully fetched all blocks")
	}
}
