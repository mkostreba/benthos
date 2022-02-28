package serverless

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Jeffail/benthos/v3/internal/bundle"
	imetrics "github.com/Jeffail/benthos/v3/internal/component/metrics"
	ioutput "github.com/Jeffail/benthos/v3/internal/component/output"
	"github.com/Jeffail/benthos/v3/internal/component/processor"
	"github.com/Jeffail/benthos/v3/internal/config"
	"github.com/Jeffail/benthos/v3/internal/manager"
	"github.com/Jeffail/benthos/v3/internal/manager/mock"
	"github.com/Jeffail/benthos/v3/internal/pipeline"
	"github.com/Jeffail/benthos/v3/lib/log"
	"github.com/Jeffail/benthos/v3/lib/message"
	"github.com/Jeffail/benthos/v3/lib/message/roundtrip"
	"github.com/Jeffail/benthos/v3/lib/metrics"
	"github.com/Jeffail/benthos/v3/lib/output"
	"github.com/Jeffail/benthos/v3/lib/response"
	"github.com/Jeffail/benthos/v3/lib/tracer"
)

// ServerlessResponseType is an output type that redirects pipeline outputs back
// to the handler.
const ServerlessResponseType = "sync_response"

// Handler contains a live Benthos pipeline and wraps it within an invoke
// handler.
type Handler struct {
	transactionChan chan message.Transaction
	done            func(exitTimeout time.Duration) error
}

// Close shuts down the underlying pipeline. If the shut down takes longer than
// the specified timeout it is aborted and an error is returned.
func (h *Handler) Close(tout time.Duration) error {
	return h.done(tout)
}

// Handle is a request/response func that injects a payload into the underlying
// Benthos pipeline and returns a result.
func (h *Handler) Handle(ctx context.Context, obj interface{}) (interface{}, error) {
	msg := message.QuickBatch(nil)
	part := message.NewPart(nil)
	if err := part.SetJSON(obj); err != nil {
		return nil, err
	}
	msg.Append(part)

	store := roundtrip.NewResultStore()
	roundtrip.AddResultStore(msg, store)

	resChan := make(chan response.Error, 1)

	select {
	case h.transactionChan <- message.NewTransaction(msg, resChan):
	case <-ctx.Done():
		return nil, errors.New("request cancelled")
	}

	select {
	case res := <-resChan:
		if res.AckError() != nil {
			return nil, res.AckError()
		}
	case <-ctx.Done():
		return nil, errors.New("request cancelled")
	}

	resultBatches := store.Get()
	if len(resultBatches) == 0 {
		return map[string]interface{}{"message": "request successful"}, nil
	}

	lambdaResults := make([][]interface{}, len(resultBatches))
	for i, batch := range resultBatches {
		batchResults := make([]interface{}, batch.Len())
		if err := batch.Iter(func(j int, p *message.Part) error {
			var merr error
			if batchResults[j], merr = p.JSON(); merr != nil {
				return fmt.Errorf("failed to marshal json response: %v", merr)
			}
			return nil
		}); err != nil {
			return nil, fmt.Errorf("failed to process result batch '%v': %v", i, err)
		}
		lambdaResults[i] = batchResults
	}

	if len(lambdaResults) == 1 {
		if len(lambdaResults[0]) == 1 {
			return lambdaResults[0][0], nil
		}
		return lambdaResults[0], nil
	}

	genBatchOfBatches := make([]interface{}, len(lambdaResults))
	for i, b := range lambdaResults {
		genBatchOfBatches[i] = b
	}
	return genBatchOfBatches, nil
}

// NewHandler returns a Handler by creating a Benthos pipeline.
func NewHandler(conf config.Type) (*Handler, error) {
	// Logging and stats aggregation.
	logger, err := log.NewV2(os.Stdout, conf.Logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create logger: %v", err)
	}

	// Create our metrics type.
	var stats *imetrics.Namespaced
	if stats, err = bundle.AllMetrics.Init(conf.Metrics, logger); err != nil {
		logger.Errorf("Failed to connect metrics aggregator: %v\n", err)
		stats = imetrics.NewNamespaced(metrics.Noop())
	}

	// Create our tracer type.
	var trac tracer.Type
	if trac, err = bundle.AllTracers.Init(conf.Tracer); err != nil {
		logger.Errorf("Failed to initialise tracer: %v\n", err)
		trac = tracer.Noop()
	}

	// Create resource manager.
	manager, err := manager.NewV2(conf.ResourceConfig, mock.NewManager(), logger, stats)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %v", err)
	}

	// Create pipeline and output layers.
	var pipelineLayer processor.Pipeline
	var outputLayer ioutput.Streamed

	transactionChan := make(chan message.Transaction, 1)

	pMgr := manager.IntoPath("pipeline")
	if pipelineLayer, err = pipeline.New(conf.Pipeline, pMgr); err != nil {
		return nil, fmt.Errorf("failed to create resource pipeline: %w", err)
	}

	oMgr := manager.IntoPath("output")
	if outputLayer, err = output.New(conf.Output, oMgr, oMgr.Logger(), oMgr.Metrics()); err != nil {
		return nil, fmt.Errorf("failed to create resource output: %w", err)
	}

	if err = pipelineLayer.Consume(transactionChan); err != nil {
		return nil, fmt.Errorf("failed to create resource: %v", err)
	}

	if err = outputLayer.Consume(pipelineLayer.TransactionChan()); err != nil {
		return nil, fmt.Errorf("failed to create resource: %v", err)
	}

	return &Handler{
		transactionChan: transactionChan,
		done: func(exitTimeout time.Duration) error {
			timesOut := time.Now().Add(exitTimeout)
			pipelineLayer.CloseAsync()
			outputLayer.CloseAsync()

			if err = outputLayer.WaitForClose(exitTimeout); err != nil {
				return fmt.Errorf("failed to cleanly close output layer: %v", err)
			}
			if err = pipelineLayer.WaitForClose(time.Until(timesOut)); err != nil {
				return fmt.Errorf("failed to cleanly close pipeline layer: %v", err)
			}

			manager.CloseAsync()
			if err = manager.WaitForClose(time.Until(timesOut)); err != nil {
				return fmt.Errorf("failed to cleanly close resources: %v", err)
			}

			trac.Close()

			if sCloseErr := stats.Close(); sCloseErr != nil {
				logger.Errorf("Failed to cleanly close metrics aggregator: %v\n", sCloseErr)
			}
			return nil
		},
	}, nil
}

//------------------------------------------------------------------------------