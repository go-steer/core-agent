// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package attach

import "github.com/go-steer/core-agent/v3/pkg/attach/wire"

// The protocol vocabulary — typed event payloads, event names, durable
// eventlog rows, the turn-error classifier, and the capability
// interfaces the agent implements — lives in pkg/attach/wire so that
// pkg/agent can emit it without linking this package's HTTP/SSE server
// (#1415). Every moved name is re-exported here under its original
// identifier: a type alias carries its methods, a const or var carries
// its value, so existing callers of attach.X compile unchanged and the
// public surface of this package is the same as before the split. New
// vocabulary goes in wire first and gets a line here.

// SSE event-type names. See wire.EventStatusUpdate and friends.
const (
	EventCapabilities  = wire.EventCapabilities
	EventStatusUpdate  = wire.EventStatusUpdate
	EventUsageUpdate   = wire.EventUsageUpdate
	EventInbox         = wire.EventInbox
	EventTurnComplete  = wire.EventTurnComplete
	EventTurnError     = wire.EventTurnError
	EventPause         = wire.EventPause
	EventWake          = wire.EventWake
	EventGuardrailTrip = wire.EventGuardrailTrip
	EventAgent         = wire.EventAgent
)

// Turn-state values. See wire.TurnStateIdle and friends.
const (
	TurnStateIdle               = wire.TurnStateIdle
	TurnStateStreaming          = wire.TurnStateStreaming
	TurnStateAwaitingPermission = wire.TurnStateAwaitingPermission
	TurnStateAwaitingElicit     = wire.TurnStateAwaitingElicit
)

// Inbox, pause and resume vocabularies. See the wire package.
const (
	InboxStateQueued   = wire.InboxStateQueued
	InboxStateDequeued = wire.InboxStateDequeued

	PauseStatePaused  = wire.PauseStatePaused
	PauseStateResumed = wire.PauseStateResumed

	ResumeModeSteer    = wire.ResumeModeSteer
	ResumeModeContinue = wire.ResumeModeContinue
	ResumeModeAbandon  = wire.ResumeModeAbandon
)

// TurnError kinds. See wire.TurnErrorConfig and friends.
const (
	TurnErrorConfig        = wire.TurnErrorConfig
	TurnErrorAuth          = wire.TurnErrorAuth
	TurnErrorModelNotFound = wire.TurnErrorModelNotFound
	TurnErrorRateLimited   = wire.TurnErrorRateLimited
	TurnErrorTransientNet  = wire.TurnErrorTransientNet
	TurnErrorCostCeiling   = wire.TurnErrorCostCeiling
	TurnErrorWatchdog      = wire.TurnErrorWatchdog
	TurnErrorRefusalStorm  = wire.TurnErrorRefusalStorm
	TurnErrorCanceled      = wire.TurnErrorCanceled
	TurnErrorUnknown       = wire.TurnErrorUnknown
)

// Typed event payloads. See the wire package for each type's contract.
type (
	Capabilities  = wire.Capabilities
	AgentIdentity = wire.AgentIdentity
	StatusUpdate  = wire.StatusUpdate
	UsageUpdate   = wire.UsageUpdate
	UsageLastTurn = wire.UsageLastTurn
	UsageByModel  = wire.UsageByModel
	InboxEvent    = wire.InboxEvent
	PauseEvent    = wire.PauseEvent
	WakeEvent     = wire.WakeEvent
	TurnComplete  = wire.TurnComplete
	TurnError     = wire.TurnError
	GuardrailTrip = wire.GuardrailTrip
)

// Turn-error classification. See wire.ClassifyTurnError.
type SelfClassifyingError = wire.SelfClassifyingError

// ClassifyTurnError is wire.ClassifyTurnError.
var ClassifyTurnError = wire.ClassifyTurnError

// Guardrail names. See wire.GuardrailWatchdog and friends.
const (
	GuardrailWatchdog    = wire.GuardrailWatchdog
	GuardrailCostCeiling = wire.GuardrailCostCeiling
	GuardrailAll         = wire.GuardrailAll
)

// Durable guardrail rows (#643, #1258). See the wire package.
const (
	GuardrailTripEventName       = wire.GuardrailTripEventName
	GuardrailTripEventAuthor     = wire.GuardrailTripEventAuthor
	GuardrailResetEventName      = wire.GuardrailResetEventName
	GuardrailResetEventAuthor    = wire.GuardrailResetEventAuthor
	GuardrailTurnTripEventName   = wire.GuardrailTurnTripEventName
	GuardrailTurnTripEventAuthor = wire.GuardrailTurnTripEventAuthor
	TurnErrorEventName           = wire.TurnErrorEventName
	TurnErrorEventAuthor         = wire.TurnErrorEventAuthor
)

// GuardrailPersistedState is wire.GuardrailPersistedState.
type GuardrailPersistedState = wire.GuardrailPersistedState

// Guardrail and turn-error row constructors and readers. See the wire
// package for each function's contract.
var (
	NewGuardrailTripEvent       = wire.NewGuardrailTripEvent
	NewGuardrailResetAuditEvent = wire.NewGuardrailResetAuditEvent
	FoldGuardrailEvents         = wire.FoldGuardrailEvents
	NewGuardrailTurnTripEvent   = wire.NewGuardrailTurnTripEvent
	NewGuardrailHaltEvent       = wire.NewGuardrailHaltEvent
	GuardrailTurnTrip           = wire.GuardrailTurnTrip
	GuardrailHaltRow            = wire.GuardrailHaltRow
	NewTurnErrorEvent           = wire.NewTurnErrorEvent
	TurnErrorRow                = wire.TurnErrorRow
)

// Context-reduction rows (#908, #974). See the wire package.
const (
	ContextReductionFailedEventName     = wire.ContextReductionFailedEventName
	ContextReductionFailedEventAuthor   = wire.ContextReductionFailedEventAuthor
	ContextReductionCompaction          = wire.ContextReductionCompaction
	ContextReductionCheckpoint          = wire.ContextReductionCheckpoint
	ContextReductionDegradedEventName   = wire.ContextReductionDegradedEventName
	ContextReductionDegradedEventAuthor = wire.ContextReductionDegradedEventAuthor
	ContextReductionWindowUnknown       = wire.ContextReductionWindowUnknown
	ContextReductionMechanical          = wire.ContextReductionMechanical
	ContextReductionTurnCut             = wire.ContextReductionTurnCut
)

// Context-reduction row constructors and readers. See the wire package.
var (
	NewContextReductionFailedEvent   = wire.NewContextReductionFailedEvent
	ContextReductionFailure          = wire.ContextReductionFailure
	NewContextReductionDegradedEvent = wire.NewContextReductionDegradedEvent
	ContextReductionDegraded         = wire.ContextReductionDegraded
)

// Permission-mode audit row (#1168). See the wire package.
const (
	PermModeEventName   = wire.PermModeEventName
	PermModeEventAuthor = wire.PermModeEventAuthor
)

// NewPermModeAuditEvent is wire.NewPermModeAuditEvent.
var NewPermModeAuditEvent = wire.NewPermModeAuditEvent

// Operator-interrupt audit (#565). See the wire package.
type InterruptSelfAuditor = wire.InterruptSelfAuditor

// NewInterruptAuditEvent is wire.NewInterruptAuditEvent.
var NewInterruptAuditEvent = wire.NewInterruptAuditEvent

// Caller-context accessors the agent reads (#1175, #1230). See the
// wire package.
var (
	DirectCaller = wire.DirectCaller
	InjectTask   = wire.InjectTask
)

// Tool sources and subagent statuses. See the wire package.
const (
	ToolSourceBuiltin  = wire.ToolSourceBuiltin
	ToolSourceMCP      = wire.ToolSourceMCP
	ToolSourceSkill    = wire.ToolSourceSkill
	ToolSourceSubagent = wire.ToolSourceSubagent
	ToolSourceOther    = wire.ToolSourceOther

	AgentStatusRunning   = wire.AgentStatusRunning
	AgentStatusCompleted = wire.AgentStatusCompleted
	AgentStatusFailed    = wire.AgentStatusFailed
	AgentStatusStopped   = wire.AgentStatusStopped
	AgentStatusDeferred  = wire.AgentStatusDeferred
)

// Agent-side projections and the capability interfaces that return
// them. See the wire package for each one's contract.
type (
	ToolInfo                = wire.ToolInfo
	AgentInfo               = wire.AgentInfo
	SubagentCatalogInfo     = wire.SubagentCatalogInfo
	AgentsProvider          = wire.AgentsProvider
	SubagentCatalogProvider = wire.SubagentCatalogProvider
	DescriptionProvider     = wire.DescriptionProvider
	SubagentSpec            = wire.SubagentSpec
	SubagentBudget          = wire.SubagentBudget
	SubagentSpawnResponse   = wire.SubagentSpawnResponse
	SubagentSpawner         = wire.SubagentSpawner
	StopAgentOutcome        = wire.StopAgentOutcome
	AgentStopReporter       = wire.AgentStopReporter
)
