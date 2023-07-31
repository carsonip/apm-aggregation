package aggregators

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"testing"

	"github.com/elastic/apm-data/input/elasticapm"
	"github.com/elastic/apm-data/model/modelpb"
	"github.com/elastic/apm-data/model/modelprocessor"
	"go.uber.org/zap"
	"golang.org/x/sync/semaphore"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func ndjsonToBatch(reader io.Reader) (*modelpb.Batch, error) {
	logger, err := zap.NewDevelopment()
	if err != nil {
		return nil, err
	}
	elasticapmProcessor := elasticapm.NewProcessor(elasticapm.Config{
		Logger:       logger,
		MaxEventSize: 1024 * 1024, // 1MiB
		Semaphore:    semaphore.NewWeighted(1),
	})
	baseEvent := modelpb.APMEvent{
		Event: &modelpb.Event{
			Received: timestamppb.Now(),
		},
	}
	var batch modelpb.Batch
	processor := modelprocessor.Chained{
		modelprocessor.SetHostHostname{},
		modelprocessor.SetServiceNodeName{},
		modelprocessor.SetGroupingKey{},
		modelprocessor.SetErrorMessage{},
		modelpb.ProcessBatchFunc(func(ctx context.Context, b *modelpb.Batch) error {
			batch = make(modelpb.Batch, len(*b))
			copy(batch, *b)
			return nil
		}),
	}

	var elasticapmResult elasticapm.Result
	if err := elasticapmProcessor.HandleStream(
		context.TODO(),
		false, // async
		&baseEvent,
		reader,
		math.MaxInt32, // batch size
		processor,
		&elasticapmResult,
	); err != nil {
		return nil, fmt.Errorf("stream error: %w", err)
	}
	return &batch, nil
}

func BenchmarkNDJSONSerial(b *testing.B) {
	dirFS := os.DirFS("testdata")
	matches, err := fs.Glob(dirFS, "*.ndjson")
	if err != nil {
		b.Fatal(err)
	}
	for _, filename := range matches {
		b.Run(filename, func(b *testing.B) {
			f, err := dirFS.Open(filename)
			if err != nil {
				b.Fatal(err)
			}
			defer f.Close()

			batch, err := ndjsonToBatch(bufio.NewReader(f))
			if err != nil {
				b.Fatal(err)
			}

			agg := newTestAggregator(b)
			b.Cleanup(func() {
				agg.Close(context.TODO())
			})
			cmID := EncodeToCombinedMetricsKeyID(b, "ab01")
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if err := agg.AggregateBatch(context.Background(), cmID, batch); err != nil {
					b.Fatal(err)
				}
			}

		})
	}
}

func BenchmarkNDJSONParallel(b *testing.B) {
	dirFS := os.DirFS("testdata")
	matches, err := fs.Glob(dirFS, "*.ndjson")
	if err != nil {
		b.Fatal(err)
	}
	for _, filename := range matches {
		b.Run(filename, func(b *testing.B) {
			f, err := dirFS.Open(filename)
			if err != nil {
				b.Fatal(err)
			}
			defer f.Close()

			batch, err := ndjsonToBatch(bufio.NewReader(f))
			if err != nil {
				b.Fatal(err)
			}

			agg := newTestAggregator(b)
			b.Cleanup(func() {
				agg.Close(context.TODO())
			})
			cmID := EncodeToCombinedMetricsKeyID(b, "ab01")
			b.ResetTimer()

			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if err := agg.AggregateBatch(context.Background(), cmID, batch); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}
