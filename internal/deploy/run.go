package deploy

import (
	"context"
	"fmt"
	"math/big"
	"reflect"
	"sync"

	"github.com/vpsfreecz/confctl/internal/generation"
	"github.com/vpsfreecz/confctl/internal/health"
)

type Phase string

const (
	QueryPhase      Phase = "query"
	CopyPhase       Phase = "copy"
	ActivationPhase Phase = "activation"
	ProfilePhase    Phase = "profile"
	RebootPhase     Phase = "reboot"
	HealthPhase     Phase = "health"
)

type State uint8

const (
	NotReached State = iota
	Started
	Succeeded
	Skipped
	Noop
	Failed
	Unknown
)

type PhaseResult struct {
	State  State
	Err    error
	Reason string
}

type Outcome struct {
	Target                                        Target
	SkippedCurrent                                bool
	Query, Copy, Dry, Activation, Profile, Reboot PhaseResult
	Health                                        PhaseResult
}

type Report struct {
	Outcomes     []Outcome
	FailedCopies []string
	HealthRounds []health.Report
	RetainLog    bool
}

type BindingError struct {
	Phase          Phase
	Host, Callback string
}

func (e *BindingError) Error() string {
	return fmt.Sprintf("deploy %s %s: invalid or missing %s binding", e.Phase, e.Host, e.Callback)
}

type OperationError struct {
	Phase        Phase
	Host, Reason string
}

func (e *OperationError) Error() string { return e.Reason }

type ConfirmationKind uint8

const (
	ConfirmCopy ConfirmationKind = iota
	ConfirmReboot
	ConfirmHealth
)

type Confirmation struct {
	Kind       ConfirmationKind
	Target     *Target
	Scope      []Machine
	CheckCount int
}

type ActivationChoice uint8

const (
	ActivationContinue ActivationChoice = iota
	ActivationRetry
	ActivationSkip
	ActivationAbort
)

type HealthChoice uint8

const (
	HealthContinue HealthChoice = iota
	HealthRetryAll
	HealthRetryFailed
	HealthAbort
)

type CopyProgress struct {
	Path           string
	Current, Total *big.Int
}

type EventKind string

const (
	QueryStarted        EventKind = "query-started"
	QueryFinished       EventKind = "query-finished"
	CurrentSkipped      EventKind = "current-skipped"
	CopyingToHost       EventKind = "copying-to-host"
	CopyStarted         EventKind = "copy-started"
	CopyAdvanced        EventKind = "copy-progress"
	CopyFinished        EventKind = "copy-finished"
	CopyFailed          EventKind = "copy-failed"
	CopySkipped         EventKind = "copy-skipped"
	DryStarted          EventKind = "dry-started"
	DryFinished         EventKind = "dry-finished"
	ActivationAdmitted  EventKind = "activation-admitted"
	ActivationStarted   EventKind = "activation-started"
	ActivationFinished  EventKind = "activation-finished"
	ActivationSkipped   EventKind = "activation-skipped"
	ActivationNoop      EventKind = "activation-noop"
	ProfileStarted      EventKind = "profile-started"
	ProfileFinished     EventKind = "profile-finished"
	RebootAnnounced     EventKind = "reboot-announced"
	RebootSkipped       EventKind = "reboot-skipped"
	RebootStarted       EventKind = "reboot-started"
	RebootFinished      EventKind = "reboot-finished"
	HealthAnnounced     EventKind = "health-announced"
	HealthRoundStarted  EventKind = "health-round-started"
	HealthRoundFinished EventKind = "health-round-finished"
)

type Event struct {
	Kind                     EventKind
	Phase                    Phase
	Host, Generation, Reason string
	Action                   Action
	Progress                 *CopyProgress
	Scope                    []Machine
	Indices                  []int
	Round, CheckCount        int
}

type Deps struct {
	QueryTarget          func(context.Context, Machine) (Status, error)
	CarrierName          func(Machine) (string, error)
	AutoRollbackDefault  func(Machine) (bool, error)
	Copy                 func(context.Context, Machine, []string, func(CopyProgress)) (bool, error)
	Activate             func(context.Context, Machine, *generation.Build, Action) (bool, error)
	ActivateWithRollback func(context.Context, Machine, *generation.Build, Action) (bool, error)
	SetProfile           func(context.Context, Machine, string) (bool, error)
	SetCarriedProfile    func(context.Context, Machine, string) (bool, error)
	RebootAndWait        func(context.Context, Machine, WaitMode) error
	Checks               func([]Machine) ([]health.Check, error)
	RunHealth            func(context.Context, []Machine, []health.Check) (health.Report, error)
	Confirm              func(context.Context, Confirmation) (bool, error)
	ChooseActivation     func(context.Context, Target, bool) (ActivationChoice, error)
	ChooseHealth         func(context.Context, []Machine, health.Report) (HealthChoice, error)
	WithActivation       func(context.Context, Target, func() error) error
	WithBulkCopy         func(context.Context, []Target, func() error) error
	Event                func(Event)
}

type runner struct {
	plan     Plan
	d        Deps
	report   Report
	machines map[string]Machine
	outcome  map[string]int
	eventMu  sync.Mutex
}

func (r *runner) event(e Event) {
	if r.d.Event == nil {
		return
	}
	if e.Progress != nil {
		p := *e.Progress
		p.Current, p.Total = copyInteger(p.Current), copyInteger(p.Total)
		e.Progress = &p
	}
	e.Scope = append([]Machine(nil), e.Scope...)
	e.Indices = append([]int(nil), e.Indices...)
	r.eventMu.Lock()
	defer r.eventMu.Unlock()
	r.d.Event(e)
}

func (r *runner) result(t Target) *Outcome             { return &r.report.Outcomes[r.outcome[t.Host]] }
func binding(phase Phase, host, callback string) error { return &BindingError{phase, host, callback} }
func ordinary(phase Phase, host, reason string) error  { return &OperationError{phase, host, reason} }
func completion(success bool, err error) PhaseResult {
	if err != nil {
		return PhaseResult{State: Unknown, Err: err}
	}
	if success {
		return PhaseResult{State: Succeeded}
	}
	return PhaseResult{State: Failed}
}

func Run(ctx context.Context, plan Plan, deps Deps) (Report, error) {
	if !plan.prepared {
		return Report{}, &InputError{"plan must be prepared"}
	}
	r := &runner{plan: plan, d: deps, machines: map[string]Machine{}, outcome: map[string]int{}}
	for _, m := range plan.machines {
		r.machines[m.Name] = m
	}
	for i, t := range plan.targets {
		r.outcome[t.Host] = i
		r.report.Outcomes = append(r.report.Outcomes, Outcome{Target: t})
	}
	targets := append([]Target(nil), plan.targets...)
	machines := append([]Machine(nil), plan.machines...)
	if err := ctx.Err(); err != nil {
		return r.report, err
	}
	if len(targets) == 0 {
		return r.report, nil
	}
	if !plan.options.CopyOnly {
		statuses, err := r.query(ctx, targets)
		if err != nil {
			return r.report, err
		}
		active := map[string]bool{}
		kept := targets[:0]
		for i, t := range targets {
			if isCurrent(r.machines[t.Host], t.Generation, statuses[i], plan.options.Action) {
				o := r.result(t)
				o.SkippedCurrent = true
				o.Copy, o.Activation = PhaseResult{State: Skipped, Reason: "current"}, PhaseResult{State: Skipped, Reason: "current"}
				r.event(Event{Kind: CurrentSkipped, Host: t.Host, Generation: t.Generation.Name})
			} else {
				kept = append(kept, t)
				active[t.Host] = true
			}
		}
		targets = kept
		filtered := machines[:0]
		for _, m := range machines {
			if active[m.Name] {
				filtered = append(filtered, m)
			}
		}
		machines = filtered
		if len(targets) == 0 {
			return r.report, nil
		}
	}
	if plan.options.OneByOne {
		for _, t := range targets {
			if err := r.copyOne(ctx, t); err != nil {
				return r.report, err
			}
			if plan.options.CopyOnly || r.result(t).Copy.State == Skipped {
				continue
			}
			skipped, err := r.activate(ctx, t)
			if err != nil {
				return r.report, err
			}
			if skipped {
				continue
			}
			if plan.options.Reboot {
				skipped, err = r.reboot(ctx, t)
				if err != nil {
					return r.report, err
				}
				if skipped {
					continue
				}
			}
			if r.wantHealth() {
				if err := r.health(ctx, []Machine{r.machines[t.Host]}); err != nil {
					return r.report, err
				}
			}
		}
		return r.report, nil
	}
	if plan.options.Interactive {
		for _, t := range targets {
			if err := r.copyOne(ctx, t); err != nil {
				return r.report, err
			}
		}
	} else {
		queue, err := r.copyQueue(targets, machines)
		if err != nil {
			return r.report, err
		}
		if err := r.withBulkCopy(ctx, queue); err != nil {
			return r.report, err
		}
	}
	if plan.options.CopyOnly {
		return r.report, nil
	}
	skipped := map[string]bool{}
	for _, t := range targets {
		if r.result(t).Copy.State == Skipped {
			r.result(t).Activation = PhaseResult{State: Skipped, Reason: "copy skipped"}
			r.event(Event{Kind: ActivationSkipped, Host: t.Host, Reason: "copy skipped"})
			skipped[t.Host] = true
			continue
		}
		skip, err := r.activate(ctx, t)
		if err != nil {
			return r.report, err
		}
		if skip {
			skipped[t.Host] = true
		}
	}
	if plan.options.Reboot {
		for _, t := range targets {
			if skipped[t.Host] {
				continue
			}
			if _, err := r.reboot(ctx, t); err != nil {
				return r.report, err
			}
		}
	}
	if r.wantHealth() {
		scope := []Machine{}
		for _, m := range machines {
			if !skipped[m.Name] {
				scope = append(scope, m)
			}
		}
		if err := r.health(ctx, scope); err != nil {
			return r.report, err
		}
	}
	return r.report, nil
}

func isCurrent(machine Machine, build *generation.Build, status Status, action Action) bool {
	if status.CurrentToplevel == nil || *status.CurrentToplevel != build.Toplevel {
		return false
	}
	if machine.Carried || action == Test || action == DryActivate {
		return true
	}
	if status.Generations == nil {
		return false
	}
	current := status.Generations.Current()
	return current != nil && current.Toplevel != nil && *current.Toplevel == build.Toplevel
}

// Query and copy have separate, phase-local schedulers. Both stop dequeueing on
// infrastructure failure, cancel their owned phase and join callbacks. A normal
// false copy result deliberately does not cancel the remaining work.
func (r *runner) query(parent context.Context, targets []Target) ([]Status, error) {
	statuses := make([]Status, len(targets))
	if r.d.QueryTarget == nil {
		return statuses, binding(QueryPhase, "", "QueryTarget")
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	workers := len(targets)
	if workers > 5 {
		workers = 5
	}
	var mu sync.Mutex
	var group sync.WaitGroup
	next := 0
	var first error
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				mu.Lock()
				if first != nil || ctx.Err() != nil || next == len(targets) {
					mu.Unlock()
					return
				}
				i := next
				next++
				mu.Unlock()
				t := targets[i]
				r.result(t).Query = PhaseResult{State: Started}
				r.event(Event{Kind: QueryStarted, Phase: QueryPhase, Host: t.Host})
				status, err := r.d.QueryTarget(ctx, r.machines[t.Host])
				r.result(t).Query = completion(true, err)
				mu.Lock()
				statuses[i] = status
				if err != nil && first == nil {
					first = err
					cancel()
				}
				mu.Unlock()
				r.event(Event{Kind: QueryFinished, Phase: QueryPhase, Host: t.Host})
			}
		}()
	}
	group.Wait()
	if first != nil {
		return statuses, first
	}
	return statuses, ctx.Err()
}

func (r *runner) copyQueue(targets []Target, machines []Machine) ([]Target, error) {
	byHost := make(map[string]Target, len(targets))
	for _, t := range targets {
		byHost[t.Host] = t
	}
	var queue []Target
	var carriers []string
	groups := map[string][]Machine{}
	count := 0
	appendTarget := func(m Machine) {
		count++
		if t, ok := byHost[m.Name]; ok {
			queue = append(queue, t)
		}
	}
	for _, m := range machines {
		if !m.Carried {
			appendTarget(m)
			continue
		}
		if r.d.CarrierName == nil {
			return nil, binding(CopyPhase, m.Name, "CarrierName")
		}
		name, err := r.d.CarrierName(m)
		if err != nil {
			return nil, err
		}
		if _, exists := groups[name]; !exists {
			carriers = append(carriers, name)
		}
		groups[name] = append(groups[name], m)
	}
	for round := 0; ; round++ {
		added := false
		for _, carrier := range carriers {
			if round < len(groups[carrier]) {
				appendTarget(groups[carrier][round])
				added = true
			}
		}
		if !added {
			break
		}
	}
	if count != len(targets) || len(queue) != len(targets) {
		return nil, ordinary(CopyPhase, "", "programming error: sorted generations length != original generations")
	}
	return queue, nil
}

func (r *runner) rollback(machine Machine, build *generation.Build, phase Phase) (bool, error) {
	o := r.plan.options
	if o.DisableAutoRollback {
		return false, nil
	}
	enabled := o.EnableAutoRollback
	if !enabled {
		if r.d.AutoRollbackDefault == nil {
			return false, binding(phase, machine.Name, "AutoRollbackDefault")
		}
		var err error
		enabled, err = r.d.AutoRollbackDefault(machine)
		if err != nil {
			return false, err
		}
	}
	return enabled && build.AutoRollback != nil, nil
}

func (r *runner) paths(t Target) ([]string, error) {
	paths := []string{t.Generation.Toplevel}
	rollback, err := r.rollback(r.machines[t.Host], t.Generation, CopyPhase)
	if err != nil {
		return nil, err
	}
	if rollback {
		paths = append(paths, *t.Generation.AutoRollback)
	}
	return paths, nil
}

func (r *runner) copyCall(ctx context.Context, t Target) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	paths, err := r.paths(t)
	if err != nil {
		return err
	}
	if r.d.Copy == nil {
		return binding(CopyPhase, t.Host, "Copy")
	}
	o := r.result(t)
	o.Copy = PhaseResult{State: Started}
	r.event(Event{Kind: CopyStarted, Phase: CopyPhase, Host: t.Host})
	success, err := r.d.Copy(ctx, r.machines[t.Host], paths, func(p CopyProgress) {
		r.event(Event{Kind: CopyAdvanced, Phase: CopyPhase, Host: t.Host, Progress: &p})
	})
	o.Copy = completion(success, err)
	if success && err == nil && r.machines[t.Host].Localhost {
		o.Copy = PhaseResult{State: Noop, Reason: "localhost"}
	}
	return err
}

func (r *runner) confirm(ctx context.Context, confirmation Confirmation) (bool, error) {
	if r.d.Confirm == nil {
		return false, binding(phaseForConfirmation(confirmation.Kind), "", "Confirm")
	}
	if confirmation.Target != nil {
		target := *confirmation.Target
		confirmation.Target = &target
	}
	confirmation.Scope = append([]Machine(nil), confirmation.Scope...)
	return r.d.Confirm(ctx, confirmation)
}

func phaseForConfirmation(kind ConfirmationKind) Phase {
	switch kind {
	case ConfirmCopy:
		return CopyPhase
	case ConfirmReboot:
		return RebootPhase
	default:
		return HealthPhase
	}
}

func (r *runner) copyOne(ctx context.Context, t Target) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.event(Event{Kind: CopyingToHost, Phase: CopyPhase, Host: t.Host})
	if r.plan.options.Interactive {
		ok, err := r.confirm(ctx, Confirmation{Kind: ConfirmCopy, Target: &t})
		if err != nil {
			return err
		}
		if !ok {
			r.result(t).Copy = PhaseResult{State: Skipped, Reason: "declined"}
			r.event(Event{Kind: CopySkipped, Phase: CopyPhase, Host: t.Host})
			return nil
		}
	}
	err := r.copyCall(ctx, t)
	r.event(Event{Kind: CopyFinished, Phase: CopyPhase, Host: t.Host})
	if err != nil {
		return err
	}
	if r.result(t).Copy.State == Failed {
		r.report.FailedCopies = append(r.report.FailedCopies, t.Host)
		r.event(Event{Kind: CopyFailed, Phase: CopyPhase, Host: t.Host})
		return ordinary(CopyPhase, t.Host, "copy to "+t.Host+" failed")
	}
	return nil
}

func (r *runner) withBulkCopy(ctx context.Context, queue []Target) error {
	calls := 0
	var bodyErr error
	body := func() error {
		calls++
		if calls != 1 {
			return binding(CopyPhase, "", "WithBulkCopy body count")
		}
		bodyErr = r.bulkCopy(ctx, queue)
		return bodyErr
	}
	if r.d.WithBulkCopy == nil {
		return body()
	}
	err := r.d.WithBulkCopy(ctx, append([]Target(nil), queue...), body)
	if err != nil {
		return err
	}
	if calls != 1 {
		return binding(CopyPhase, "", "WithBulkCopy body count")
	}
	return bodyErr
}

func (r *runner) bulkCopy(parent context.Context, queue []Target) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var mu sync.Mutex
	var group sync.WaitGroup
	next := 0
	var first error
	for worker := 0; worker < copyWorkerCount(r.plan.options.CopyWorkers, len(queue)); worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				mu.Lock()
				if first != nil || ctx.Err() != nil || next == len(queue) {
					mu.Unlock()
					return
				}
				t := queue[next]
				next++
				mu.Unlock()
				r.event(Event{Kind: CopyingToHost, Phase: CopyPhase, Host: t.Host})
				err := r.copyCall(ctx, t)
				mu.Lock()
				if err != nil && first == nil {
					first = err
					cancel()
				}
				if err == nil && r.result(t).Copy.State == Failed {
					r.report.FailedCopies = append(r.report.FailedCopies, t.Host)
				}
				mu.Unlock()
				r.event(Event{Kind: CopyFinished, Phase: CopyPhase, Host: t.Host})
			}
		}()
	}
	group.Wait()
	for _, host := range r.report.FailedCopies {
		r.event(Event{Kind: CopyFailed, Phase: CopyPhase, Host: host})
	}
	if first != nil {
		return first
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(r.report.FailedCopies) > 0 {
		return ordinary(CopyPhase, "", "Copy to some hosts failed")
	}
	return nil
}

func (r *runner) activate(ctx context.Context, t Target) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	calls, skipped := 0, false
	var bodyErr error
	body := func() error {
		calls++
		if calls != 1 {
			return binding(ActivationPhase, t.Host, "WithActivation body count")
		}
		skipped, bodyErr = r.activationBody(ctx, t)
		return bodyErr
	}
	if r.d.WithActivation == nil {
		err := body()
		return skipped, err
	}
	err := r.d.WithActivation(ctx, t, body)
	if err != nil {
		return skipped, err
	}
	if calls != 1 {
		return skipped, binding(ActivationPhase, t.Host, "WithActivation body count")
	}
	return skipped, bodyErr
}

func (r *runner) activationBody(ctx context.Context, t Target) (bool, error) {
	m, o := r.machines[t.Host], r.result(t)
	action := r.plan.options.Action
	if m.Carried {
		o.Activation = PhaseResult{State: Noop, Reason: "carried"}
		r.event(Event{Kind: ActivationNoop, Phase: ActivationPhase, Host: t.Host, Action: action})
		if action != Switch {
			return false, nil
		}
		if r.d.SetCarriedProfile == nil {
			return false, binding(ProfilePhase, t.Host, "SetCarriedProfile")
		}
		o.Profile = PhaseResult{State: Started}
		r.event(Event{Kind: ProfileStarted, Phase: ProfilePhase, Host: t.Host})
		ok, err := r.d.SetCarriedProfile(ctx, m, t.Generation.Toplevel)
		o.Profile = completion(ok, err)
		r.event(Event{Kind: ProfileFinished, Phase: ProfilePhase, Host: t.Host})
		if err != nil {
			return false, err
		}
		if !ok {
			return false, ordinary(ProfilePhase, t.Host, "carried profile update failed")
		}
		return false, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if r.plan.options.DryActivateFirst {
			if r.d.Activate == nil {
				return false, binding(ActivationPhase, t.Host, "Activate")
			}
			o.Dry = PhaseResult{State: Started}
			r.event(Event{Kind: DryStarted, Phase: ActivationPhase, Host: t.Host, Action: DryActivate})
			ok, err := r.d.Activate(ctx, m, t.Generation, DryActivate)
			o.Dry = completion(ok, err)
			r.event(Event{Kind: DryFinished, Phase: ActivationPhase, Host: t.Host, Action: DryActivate})
			if err != nil {
				return false, err
			}
			if !ok {
				return false, ordinary(ActivationPhase, t.Host, "dry activation failed")
			}
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		r.event(Event{Kind: ActivationAdmitted, Phase: ActivationPhase, Host: t.Host, Action: action})
		if !r.plan.options.Interactive {
			break
		}
		if r.d.ChooseActivation == nil {
			return false, binding(ActivationPhase, t.Host, "ChooseActivation")
		}
		choice, err := r.d.ChooseActivation(ctx, t, r.plan.options.DryActivateFirst)
		if err != nil {
			return false, err
		}
		switch choice {
		case ActivationContinue:
			goto admitted
		case ActivationRetry:
			if !r.plan.options.DryActivateFirst {
				return false, binding(ActivationPhase, t.Host, "retry without dry-first")
			}
		case ActivationSkip:
			o.Activation = PhaseResult{State: Skipped, Reason: "choice"}
			r.event(Event{Kind: ActivationSkipped, Phase: ActivationPhase, Host: t.Host})
			return true, nil
		case ActivationAbort:
			return false, ordinary(ActivationPhase, t.Host, "Aborted")
		default:
			return false, binding(ActivationPhase, t.Host, "ChooseActivation result")
		}
	}
admitted:
	if err := ctx.Err(); err != nil {
		return false, err
	}
	rollback := false
	var err error
	if action == Switch || action == Test {
		rollback, err = r.rollback(m, t.Generation, ActivationPhase)
		if err != nil {
			return false, err
		}
	}
	activate := r.d.Activate
	callback := "Activate"
	if rollback {
		activate, callback = r.d.ActivateWithRollback, "ActivateWithRollback"
	}
	if activate == nil {
		return false, binding(ActivationPhase, t.Host, callback)
	}
	o.Activation = PhaseResult{State: Started}
	r.event(Event{Kind: ActivationStarted, Phase: ActivationPhase, Host: t.Host, Action: action})
	ok, err := activate(ctx, m, t.Generation, action)
	o.Activation = completion(ok, err)
	r.event(Event{Kind: ActivationFinished, Phase: ActivationPhase, Host: t.Host, Action: action})
	if err != nil {
		return false, err
	}
	if !ok {
		return false, ordinary(ActivationPhase, t.Host, "activation failed")
	}
	if action != Boot && action != Switch {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if r.d.SetProfile == nil {
		return false, binding(ProfilePhase, t.Host, "SetProfile")
	}
	o.Profile = PhaseResult{State: Started}
	r.event(Event{Kind: ProfileStarted, Phase: ProfilePhase, Host: t.Host})
	ok, err = r.d.SetProfile(ctx, m, t.Generation.Toplevel)
	o.Profile = completion(ok, err)
	r.event(Event{Kind: ProfileFinished, Phase: ProfilePhase, Host: t.Host})
	if err != nil {
		return false, err
	}
	if !ok {
		return false, ordinary(ProfilePhase, t.Host, "profile update failed")
	}
	return false, nil
}

func (r *runner) reboot(ctx context.Context, t Target) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	m := r.machines[t.Host]
	reason := ""
	if m.Localhost {
		reason = "localhost"
	} else if m.Carried {
		reason = "carried"
	}
	if reason == "" {
		r.event(Event{Kind: RebootAnnounced, Phase: RebootPhase, Host: t.Host})
		if r.plan.options.Interactive {
			ok, err := r.confirm(ctx, Confirmation{Kind: ConfirmReboot, Target: &t})
			if err != nil {
				return false, err
			}
			if !ok {
				reason = "declined"
			}
		}
	}
	if reason != "" {
		r.result(t).Reboot = PhaseResult{State: Skipped, Reason: reason}
		r.event(Event{Kind: RebootSkipped, Phase: RebootPhase, Host: t.Host, Reason: reason})
		return true, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if r.d.RebootAndWait == nil {
		return false, binding(RebootPhase, t.Host, "RebootAndWait")
	}
	o := r.result(t)
	o.Reboot = PhaseResult{State: Started}
	r.event(Event{Kind: RebootStarted, Phase: RebootPhase, Host: t.Host})
	err := r.d.RebootAndWait(ctx, m, copyWait(r.plan.options.Wait))
	o.Reboot = completion(err == nil, err)
	r.event(Event{Kind: RebootFinished, Phase: RebootPhase, Host: t.Host})
	return false, err
}

func (r *runner) wantHealth() bool {
	o := r.plan.options
	return o.HealthChecks && (o.Action == Switch || o.Action == Test || o.Action == Boot && o.Reboot)
}

func (r *runner) health(ctx context.Context, scope []Machine) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	runnable := []Machine{}
	for _, m := range scope {
		if m.Runnable {
			runnable = append(runnable, m)
		}
	}
	if r.d.Checks == nil {
		return binding(HealthPhase, "", "Checks")
	}
	checks, err := r.d.Checks(runnable)
	if err != nil {
		return err
	}
	if len(checks) == 0 {
		for _, m := range scope {
			r.report.Outcomes[r.outcome[m.Name]].Health = PhaseResult{State: Noop, Reason: "no checks"}
		}
		return nil
	}
	allowed := map[string]bool{}
	for _, m := range runnable {
		allowed[m.Name] = true
	}
	for _, check := range checks {
		if !allowed[check.Machine] {
			return binding(HealthPhase, check.Machine, "Checks machine")
		}
	}
	if r.plan.options.Interactive {
		r.event(Event{Kind: HealthAnnounced, Phase: HealthPhase, Scope: scope, CheckCount: len(checks)})
		ok, err := r.confirm(ctx, Confirmation{Kind: ConfirmHealth, Scope: scope, CheckCount: len(checks)})
		if err != nil {
			return err
		}
		if !ok {
			for _, m := range scope {
				r.report.Outcomes[r.outcome[m.Name]].Health = PhaseResult{State: Skipped, Reason: "declined"}
			}
			return nil
		}
	}
	if r.d.RunHealth == nil {
		return binding(HealthPhase, "", "RunHealth")
	}
	indices := make([]int, len(checks))
	for i := range checks {
		indices[i] = i
	}
	states := make([]PhaseResult, len(checks))
	updateOutcomes := func() {
		for _, m := range scope {
			result := PhaseResult{State: Noop, Reason: "no checks"}
			for i, check := range checks {
				if check.Machine != m.Name {
					continue
				}
				candidate := states[i]
				if result.State == Unknown {
					continue
				}
				if candidate.State == Unknown || result.State == Noop || candidate.State == Failed || result.State != Failed && candidate.State == Started {
					result = candidate
				}
			}
			r.report.Outcomes[r.outcome[m.Name]].Health = result
		}
	}
	for round := 1; ; round++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		submitted := make([]health.Check, len(indices))
		for i, index := range indices {
			submitted[i] = checks[index]
		}
		r.event(Event{Kind: HealthRoundStarted, Phase: HealthPhase, Scope: scope, Indices: indices, Round: round, CheckCount: len(submitted)})
		previous := make([]PhaseResult, len(indices))
		for i, index := range indices {
			previous[i] = states[index]
			states[index] = PhaseResult{State: Started}
		}
		updateOutcomes()
		report, runErr := r.d.RunHealth(ctx, append([]Machine(nil), scope...), submitted)
		r.report.RetainLog = r.report.RetainLog || report.RetainLog
		report.Results = append([]health.Result(nil), report.Results...)
		for i := range report.Results {
			report.Results[i].Failures = append([]health.Failure(nil), report.Results[i].Failures...)
		}
		r.report.HealthRounds = append(r.report.HealthRounds, report)
		invalid := func(reason string) error {
			err := binding(HealthPhase, "", reason)
			for _, index := range indices {
				states[index] = PhaseResult{State: Unknown, Err: err}
			}
			updateOutcomes()
			if runErr != nil {
				return runErr
			}
			return err
		}
		if len(report.Results) != len(submitted) {
			return invalid("RunHealth result count")
		}
		for i, result := range report.Results {
			if !reflect.DeepEqual(result.Check, submitted[i]) ||
				result.Complete && (!result.Started || result.Err != nil || result.Attempts <= 0) ||
				!result.Started && (result.Attempts != 0 || result.HadFailure || len(result.Failures) != 0) ||
				runErr == nil && (!result.Started || !result.Complete || result.Err != nil) {
				return invalid("RunHealth result order/completion")
			}
		}
		failed := []int{}
		for i, result := range report.Results {
			index := indices[i]
			switch {
			case result.Complete:
				states[index] = PhaseResult{State: Succeeded}
				if !result.Passed() {
					states[index] = PhaseResult{State: Failed}
					failed = append(failed, index)
				}
			case !result.Started:
				// Round admission does not prove execution. Keep the earlier
				// result when a retry did not reach this original check index.
				states[index] = previous[i]
			default:
				err := result.Err
				if err == nil {
					err = runErr
				}
				states[index] = PhaseResult{State: Unknown, Err: err}
			}
		}
		updateOutcomes()
		if runErr != nil {
			return runErr
		}
		r.event(Event{Kind: HealthRoundFinished, Phase: HealthPhase, Scope: scope, Indices: indices, Round: round, CheckCount: len(submitted)})
		if len(failed) == 0 {
			return nil
		}
		if !r.plan.options.Interactive {
			if r.plan.options.KeepGoing {
				return nil
			}
			return ordinary(HealthPhase, "", "Health checks have failed")
		}
		if r.d.ChooseHealth == nil {
			return binding(HealthPhase, "", "ChooseHealth")
		}
		choice, err := r.d.ChooseHealth(ctx, append([]Machine(nil), scope...), report)
		if err != nil {
			return err
		}
		switch choice {
		case HealthContinue:
			return nil
		case HealthRetryFailed:
			indices = failed
		case HealthRetryAll:
			indices = make([]int, len(checks))
			for i := range checks {
				indices[i] = i
			}
		case HealthAbort:
			return ordinary(HealthPhase, "", "Aborted")
		default:
			return binding(HealthPhase, "", "ChooseHealth result")
		}
	}
}
