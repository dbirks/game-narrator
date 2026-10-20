package api

// The documented enum vocabularies are compared against the ones the service
// actually accepts — `games.WalkEnum` for the domain tables, `auth.Ordered` for the
// role ladder — rather than against somebody's memory of them. This is the check
// that keeps a hand-written contract from containing a tidy-looking vocabulary the
// domain has never heard of, which is the failure mode that makes a generated
// client accept what the appliance refuses and reject what it stores.
//
// It caught four such inventions while this document was being written: `narration`,
// `quest`, `hint` and `error` in text_kind; `probable` and `contested` in
// attribution_status; a whole invented detect_method list (`fixed_roi`, `edge`,
// `ocr_anchor`, `adaptive`); and `rejected` for a character, which is `retired`.

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/audio"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/capture/stack"
	"github.com/dbirks/game-narrator/internal/doctor"
	"github.com/dbirks/game-narrator/internal/dolphin"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/profileio"
	"github.com/dbirks/game-narrator/internal/restore"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/setupboard"
	"github.com/dbirks/game-narrator/internal/store"
)

// domainEnums maps a documented schema (or a "Schema.properties.member" path) onto
// the Go type name whose values it must contain exactly.
var domainEnums = map[string]string{
	"ProfileStatus":                           "ProfileStatus",
	"RightsStatus":                            "RightsStatus",
	"TextKind":                                "TextKind",
	"AttributionStatus":                       "AttributionStatus",
	"JobState":                                "JobState",
	"Region.properties.region_type":           "RegionType",
	"RegionInput.properties.region_type":      "RegionType",
	"RegionInput.properties.detect_method":    "DetectMethod",
	"GraphEdge.properties.kind":               "EdgeType",
	"GraphNode.properties.attribution_status": "AttributionStatus",
	"Region.properties.detect_method":         "DetectMethod",
	"Character.properties.status":             "CharacterStatus",
	// The correction labels (ADR-0052): the kind of mistake, the table it
	// labels and the review state each carry their own vocabulary.
	"CorrectionKind":                "CorrectionKind",
	"CorrectionTarget":              "CorrectionTarget",
	"CorrectionStatus":              "CorrectionStatus",
	"JobAttempt.properties.outcome": "AttemptOutcome",
	"Role":                          "", // the ladder lives in internal/auth, checked below
}

// literalEnums pins the vocabularies that some other package writes without a
// validated table, each with where the values come from, so they are asserted
// rather than waved through.
var literalEnums = map[string][]string{
	// The ⌘K palette (ADR-0098): the six result categories internal/api/
	// search.go writes. kindOrder is their canonical group order, and the
	// schema's enum is that list, so the test pins both to one variable.
	"SearchGroup.properties.kind":  kindOrder,
	"SearchResult.properties.kind": kindOrder,
	// internal/store/repo_publish.go writes exactly these action values and these
	// two results.
	"PublishAction.properties.action": {"publish", "archive", "rejected"},
	"PublishAction.properties.result": {"succeeded", "rejected"},
	// The restore status route (ADR-0090) answers the states internal/restore
	// writes: the page's banner renders exactly these four words.
	"RestoreStatus.properties.status": {restore.StatusNone, restore.StatusPending, restore.StatusApplied, restore.StatusFailed},
	// The 202 of the restore upload answers exactly one word (the handler's
	// view type writes it; the route's other answers are Problem objects).
	"RestoreScheduled.properties.status": {"restart_scheduled"},
	// internal/api/api.go's handleReady answers one of two words.
	"HealthStatus.properties.status": {"ok", "fail"},
	// The 202 of the one route that enqueues this kind.
	"ImportAccepted.properties.job_kind": {JobKindImportFile},
	// The 202 of the disc-import route (ADR-0066) answers which shape the
	// job will read: the allowlisted file in place, or the upload's bytes.
	"DiscImportAccepted.properties.job_kind": {JobKindImportDisc},
	"DiscImportAccepted.properties.source":   {discSourceDir, discSourceUpload},
	// internal/profileio writes the manifest schema version it understands.
	"ProfileManifest.properties.schema_version": {profileio.SchemaVersion},
	// The session runtime (Milestone 2, ADR-0024) writes these words: the frame
	// source kinds it admits, the lifecycle states it transitions and the two
	// decision modes it accepts. They live in internal/runtime as constants
	// rather than a games domain table, so they are pinned here the same way
	// the api-owned JobKindImportFile is.
	"FrameSourceKind": {runtime.SourceKindReplay, runtime.SourceKindTestPattern, runtime.SourceKindV4L2, runtime.SourceKindDolphin},
	"SessionState":    {runtime.StateStarting, runtime.StateRunning, runtime.StateStopping, runtime.StateStopped, runtime.StateFailed},
	"SessionMode":     {runtime.ModeConservative, runtime.ModePredictive},
	// The managed clip store (ADR-0073): the container the upload arrived in,
	// the two words internal/clips/extract.go's Format.String() writes.
	"Clip.properties.archive_format":         {"tar", "zip", "unknown"},
	"ClipUploaded.properties.archive_format": {"tar", "zip"},
	// The per-stage bottleneck summary (docs/spec/16 M7): the three stage
	// names internal/runtime/bottleneck.go names, in pipeline order.
	"StageLatency.properties.stage": {runtime.StageFrameToOCR, runtime.StageOCRToRanked, runtime.StageFrameToCommitted},
	// The onboarding surface (ADR-0028): the run-state vocabulary the agent
	// core writes on the run row, the tool-result statuses the executor
	// wraps (plus the store's pending row state), and the two review-item
	// vocabularies internal/api/onboarding_state.go computes.
	"OnboardingRunState.properties.state":      {string(agent.StateQueued), string(agent.StateRunning), string(agent.StateBlocked), string(agent.StateReview), string(agent.StateFailed), string(agent.StateSucceeded), string(agent.StateCancelled)},
	"OnboardingToolCallView.properties.status": {"pending", string(agent.StatusSucceeded), string(agent.StatusFailed), string(agent.StatusRefused), string(agent.StatusTimeout), string(agent.StatusCancelled)},
	"OnboardingReviewItem.properties.kind":     {"publish", "tool"},
	"OnboardingReviewItem.properties.state":    {"pending", "resolved"},
	// The setup board snapshot (ADR-0108, UX-06): the card columns
	// internal/setupboard derives, the rail's six cells and cell states it
	// writes, and the run-state vocabulary the snapshot answers verbatim.
	"SetupBoard.properties.state":          {string(agent.StateQueued), string(agent.StateRunning), string(agent.StateBlocked), string(agent.StateReview), string(agent.StateFailed), string(agent.StateSucceeded), string(agent.StateCancelled)},
	"SetupBoardCharacter.properties.stage": {string(setupboard.StageFinding), string(setupboard.StageBrief), string(setupboard.StageVoicing), string(setupboard.StageDone)},
	"SetupRailCell.properties.id":          {setupboard.RailFindGame, setupboard.RailDialogue, setupboard.RailCharacters, setupboard.RailBriefs, setupboard.RailVoices, setupboard.RailPublish},
	"SetupRailCell.properties.state":       {string(setupboard.CellPending), string(setupboard.CellActive), string(setupboard.CellDone)},
	// The voice workflow (M4, docs/spec/07): the revision lifecycle the store
	// enforces (draft → auditioning → approved → retired, one-way) and the
	// per-instance registration states the registry reconciliation writes.
	"VoiceRevision.properties.state":     {store.VoiceStateDraft, store.VoiceStateAuditioning, store.VoiceStateApproved, store.VoiceStateRetired},
	"VoiceRegistration.properties.state": {store.VoiceRegPending, store.VoiceRegReady, store.VoiceRegInvalid},
	// The audition ASR QA verdict (FR-10, ADR 0061): internal/api/voices.go's
	// auditionQAStateName writes exactly these three words; "unknown" is the
	// not-run state of the documented degraded mode.
	"VoiceAudition.properties.qa_state": {"unknown", "passed", "failed"},
	// The dev-only Dolphin bridge (docs/spec/11) writes these lifecycle states
	// in internal/dolphin/state.go; the dev API serves them verbatim in the
	// status route, so the vocabulary is pinned here like the other
	// api-owned literals.
	"DolphinStatus.properties.state": {string(dolphin.StateStopped), string(dolphin.StateLaunching), string(dolphin.StateRunning), string(dolphin.StateFailed)},
	// The global status bar's rollup (docs/design/plum-peach, ADR-0104):
	// the level vocabulary internal/api/status.go writes (unknown is the
	// not-probed state the rollup refuses to count as ok) and the component
	// ids in their canonical bar order. The setup-run state is the live
	// subset of the onboarding run states: the store's ListActiveAgentRuns
	// selects exactly these four, and the terminal states are not status data.
	"StatusHealthComponent.properties.level": {statusLevelOK, statusLevelSlow, statusLevelFail, statusLevelUnknown},
	"StatusHealthComponent.properties.id":    {statusComponents[0], statusComponents[1], statusComponents[2], statusComponents[3], statusComponents[4], statusComponents[5]},
	"StatusSetupRun.properties.state":        {string(agent.StateQueued), string(agent.StateRunning), string(agent.StateBlocked), string(agent.StateReview)},
	// The session audio half (M4, ADR-0039): the sink kinds internal/runtime
	// builds (a factory sink is named by the sink it supplies) and the speech
	// lifecycle phases the wiring and the scheduler write into
	// UtteranceState — inline strings in internal/runtime/speech.go and
	// internal/speech/scheduler.go, pinned the same way as the session states.
	"SessionAudio.properties.sink": {runtime.SinkDummy, runtime.SinkFile, runtime.SinkPipeWire},
	// The diagnostics run surface (ADR-0087): the run's lifecycle words
	// (internal/api's own diagnosticsRunning/diagnosticsDone) and the doctor's
	// status vocabulary a role row reports — the same words the CLI's report
	// prints, from internal/doctor's constants, never a copy.
	"DiagnosticsRun.properties.status":  {string(diagnosticsRunning), string(diagnosticsDone)},
	"DiagnosticsRole.properties.status": {doctor.StatusOK, doctor.StatusDegraded, doctor.StatusBlocked, doctor.StatusNotConfigured, doctor.StatusFail},
	// The output-audio product surface (ADR-0074): the same sink-kind spelling
	// the session sink uses (internal/audio aliases runtime's constants), the
	// device kinds the enumerator classifies, the selection provenance, and
	// the backends ListDevices answers with (the inline names of
	// internal/audio/backend.go).
	"AudioSelection.properties.sink_kind":      {runtime.SinkDummy, runtime.SinkFile, runtime.SinkPipeWire},
	"AudioSettingsUpdate.properties.sink_kind": {runtime.SinkDummy, runtime.SinkFile, runtime.SinkPipeWire},
	"TestToneResponse.properties.sink":         {runtime.SinkDummy, runtime.SinkFile, runtime.SinkPipeWire},
	"AudioDevice.properties.kind":              {string(audio.KindUSB), string(audio.KindHDMI), string(audio.KindBuiltin), string(audio.KindNull), string(audio.KindBluetooth), string(audio.KindUnknown)},
	"SelectionSource":                          {audio.SourceEnv, audio.SourceStore, audio.SourceDefault},
	"AudioDevices.properties.backend":          {"pipewire", "alsa", "synthetic", "none"},
	"UtteranceState.properties.phase":          {"requested", "ready", "playing", "ended", "cancelled", "skipped", "muted", "failed"},
	// The region OCR preview (docs/spec/09) writes these two vocabularies in
	// internal/api/regions_preview.go: the frame source it admits and the
	// textbox verdict the panel detector answers.
	"PreviewResult.properties.source":          {"upload", "fixture"},
	"PreviewResult.properties.textbox_verdict": {"present", "absent"},
	// The capture device surface (ADR-0078): the state words internal/capture/
	// stack reports for a device, the pixel formats the ADR-0076 policy admits,
	// and the mode provenance the selection layer writes (the operator's pin or
	// the policy's pick).
	"CaptureDevice.properties.state":         {stack.StateReady, stack.StateNoFormats, stack.StateNoMode, stack.StateEnumerationErr, stack.StateNotCapture},
	"CaptureMode.properties.pix_fmt":         {"mjpeg", "yuyv422"},
	"CaptureChosen.properties.pix_fmt":       {"mjpeg", "yuyv422"},
	"CaptureChosen.properties.source":        {stack.ModeSourceSaved, stack.ModeSourcePolicy},
	"CaptureProbe.properties.mode_source":    {stack.ModeSourceSaved, stack.ModeSourcePolicy},
	"Session.properties.capture_mode_source": {stack.ModeSourceSaved, stack.ModeSourcePolicy},
	// The context-aware assistant dock (ADR-0099): the turn roles the store's
	// assistant rows carry and the two NEEDS-YOU card kinds the loop emits
	// (assistant.question events, docs/spec/08).
	"AssistantTurnView.properties.role":     {"user", "assistant"},
	"AssistantQuestionView.properties.kind": {"question", "confirmation"},
	// The metrics snapshot (docs/spec/13 "Metrics", ADR-0106): the three
	// Prometheus kinds internal/telemetry declares.
	"MetricSnapshot.properties.type": {"counter", "gauge", "histogram"},
	// The signal answer's evidence class (ADR-0110 §1): a running v4l2 session
	// answers "session", the one-shot throwaway grab answers "oneshot" — the
	// two words internal/api/capture.go writes, pinned here the same way the
	// capture source kinds are (the doc's enum must match the writer).
	"CaptureSignal.properties.source": {signalSourceSession, signalSourceOneShot},
	// The tailnet detection's answer (ADR-0110 §4): the interface scan's word
	// and the tailscale-ip fallback's word, the two values DetectTailnet writes.
	"Tailnet.properties.source": {tailnetSourceInterface, tailnetSourceTailscale},
}

func TestOpenAPIEnumVocabulariesMatchTheDomainEnums(t *testing.T) {
	doc := openapiFixture(t)
	if _, ok := pathValue(doc, "components", "schemas", "Game").(map[string]any); !ok {
		t.Fatal("the document has no component schemas, so this check would pass vacuously")
	}

	seen := map[string][]string{}
	collect := func(sch map[string]any, where string) {
		if !strings.HasPrefix(where, "#/components/schemas/") {
			return
		}
		if values, ok := sch["enum"].([]any); ok {
			out := make([]string, 0, len(values))
			for _, v := range values {
				s, _ := v.(string)
				out = append(out, s)
			}
			seen[strings.TrimPrefix(where, "#/components/schemas/")] = out
		}
	}
	walkSchemas(doc, collect)
	if len(seen) == 0 {
		t.Fatal("no enum vocabulary was found in the component schemas")
	}

	for path, documented := range seen {
		if len(documented) == 0 {
			t.Errorf("%s documents an empty enum", path)
			continue
		}
		typeName, isDomain := domainEnums[path]
		if isDomain && typeName == "" {
			// Role: the ladder is ordered and lives in internal/auth.
			var ladder []string
			for _, role := range auth.Ordered() {
				ladder = append(ladder, role.String())
			}
			if !sameStrings(documented, ladder) {
				t.Errorf("Role documents %v, auth.Ordered is %v", documented, ladder)
			}
			continue
		}
		if isDomain {
			var actual []string
			if err := games.WalkEnum(typeName, func(v string) { actual = append(actual, v) }); err != nil {
				t.Fatalf("%s maps to games.%s: %v", path, typeName, err)
			}
			// Same members, in any order. A missing value makes a generated client
			// reject what the service stores; an extra one makes it accept what the
			// service refuses. Both are contract lies.
			if !sameStrings(documented, actual) {
				t.Errorf("%s documents %v, but games.%s is %v", path, documented, typeName, actual)
			}
			continue
		}
		want, pinned := literalEnums[path]
		if !pinned {
			t.Errorf("%s declares an enum vocabulary this test accounts for nowhere: %v\n"+
				"add it to domainEnums (the games type name) or literalEnums (naming where the values are written)",
				path, documented)
			continue
		}
		if !sameStrings(documented, want) {
			t.Errorf("%s documents %v, the writing code produces %v", path, documented, want)
		}
	}

	// Visibility, not a claim: which of the domain's fifteen vocabularies this
	// surface describes. Most belong to rows Milestone 1 does not expose (corrections,
	// edges, assertions, source types), so nothing is asserted about the direction —
	// but the list is printed where a reviewer will see it.
	t.Logf("documented vocabularies: %d; domain vocabularies: %d (%s)",
		len(seen), len(games.EnumNames()), strings.Join(games.EnumNames(), ","))
}

// TestOpenAPIRequestSchemasMatchTheHandlers is the input half of the same promise:
// where the document says a body member is required or forbidden, the handler must
// behave that way. The check is the HTTP answer, not a reflection over structs,
// because the document's promise is about behaviour.
func TestOpenAPIRequestSchemasMatchTheHandlers(t *testing.T) {
	e := newCatalogEnv(t)
	doc := openapiFixture(t)
	paths, _ := pathValue(doc, "paths").(map[string]any)

	// The complete create works, and each documented-required member dropped alone
	// does not.
	if rec := e.post(auth.RoleCurator, "/api/v1/games",
		`{"slug":"contract-full","title":"Contract Full"}`, "Idempotency-Key", "req-full"); rec.Code != http.StatusCreated {
		t.Fatalf("the complete create = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	for field, payload := range map[string]string{
		"slug":  `{"title":"Contract No Slug"}`,
		"title": `{"slug":"contract-no-title"}`,
	} {
		rec := e.post(auth.RoleCurator, "/api/v1/games", payload, "Idempotency-Key", "req-"+field)
		if rec.Code == http.StatusCreated {
			t.Errorf("POST /api/v1/games succeeded with the documented-required member %q absent: %s",
				field, rec.Body.String())
		}
	}

	// The required list names exactly the two fields the handler treats as
	// mandatory: an over-long list would promise a refusal the handler does not
	// raise, an under-long one would let a title-less row through.
	schema, _ := pathValue(paths, "/api/v1/games", "post", "requestBody", "content",
		"application/json", "schema").(map[string]any)
	var required []string
	for _, raw := range schema["required"].([]any) {
		required = append(required, raw.(string))
	}
	if !sameStrings(required, []string{"slug", "title"}) {
		t.Errorf("POST /api/v1/games documents required = %v, want [slug title]", required)
	}

	_, _, profileID, etag := newDraft(t, e, "contract-fields", "contract-fields")
	msgs := mustList(e.mustOK(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/messages?limit=1")["messages"])
	if len(msgs) == 0 {
		t.Fatal("the fixture draft has no messages")
	}
	mid := msgs[0].(map[string]any)["id"].(string)
	// A member the contract does not list is refused by name — the same DisallowUnknownFields
	// rule the document states as additionalProperties: false.
	rec := e.patch(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/messages/"+mid,
		`{"speech_text":"ok","flags":{}}`, "If-Match", etag)
	requireProblem(t, rec, http.StatusBadRequest, codeInvalidParameter)
	if !strings.Contains(rec.Body.String(), "flags") {
		t.Errorf("the refusal did not name the undocumented member: %s", rec.Body.String())
	}

	// And the documented PATCH schema really says what the handler does: one
	// writable property, no open tail.
	patchMedia, _ := pathValue(paths, "/api/v1/profiles/{id}/messages/{mid}", "patch",
		"requestBody", "content", "application/json").(map[string]any)
	patchSchema, _ := patchMedia["schema"].(map[string]any)
	props, _ := patchSchema["properties"].(map[string]any)
	if len(props) != 1 {
		t.Errorf("the PATCH message request documents %d properties (%s), want the one writable field",
			len(props), strings.Join(sortedKeysOfMap(props), ","))
	}
	if _, ok := props["speech_text"]; !ok {
		t.Errorf("the PATCH message request does not document speech_text: %s", strings.Join(sortedKeysOfMap(props), ","))
	}
	if strict, isBool := patchSchema["additionalProperties"].(bool); !isBool || strict {
		t.Errorf("the PATCH message request must document additionalProperties: false, because DisallowUnknownFields enforces exactly that: %v",
			patchSchema["additionalProperties"])
	}
	if req, _ := patchSchema["required"].([]any); len(req) != 1 || req[0] != "speech_text" {
		t.Errorf("the PATCH message request documents required = %v, want [speech_text]", req)
	}
}

func sortedKeysOfMap(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sameStrings(a, b []string) bool {
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}
