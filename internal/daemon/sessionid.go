package daemon

import (
	"context"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// consultWithSession runs a consult and reports which CLI conversation it ran
// as, so the audit row can name the transcript the run left behind.
//
// Optional capability: an adapter that cannot report one still consults
// normally and yields the id hap minted (correct for every CLI that ACCEPTS an
// id, and simply unknown for one that mints its own). That is what keeps every
// existing LLMPort fake compiling without a session stub.
func consultWithSession(ctx context.Context, llm ports.LLMPort, req domain.LLMRequest) (*domain.LLMDecision, string, error) {
	if sr, ok := llm.(ports.SessionReportingLLM); ok {
		return sr.ConsultWithSession(ctx, req)
	}
	dec, err := llm.Consult(ctx, req)
	return dec, req.SessionID, err
}

// generateTaskWithSession is the GenerateTask counterpart of
// consultWithSession. Task generation is the largest single producer of CLI
// transcripts, so it carries a session id too.
func generateTaskWithSession(ctx context.Context, tg ports.TaskGeneratorPort, req domain.TaskGenRequest) (string, string, error) {
	if sr, ok := tg.(ports.SessionReportingTaskGenerator); ok {
		return sr.GenerateTaskWithSession(ctx, req)
	}
	task, err := tg.GenerateTask(ctx, req)
	return task, req.SessionID, err
}
