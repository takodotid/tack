package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Mode is the kind of run.
// +kubebuilder:validation:Enum=Planned;SiteLoss
type Mode string

const (
	// ModePlanned moves the primary while both sites answer. The old
	// primary demotes and hands its token to the new one.
	ModePlanned Mode = "Planned"
	// ModeSiteLoss promotes the new site without a token, after the old
	// site is lost. The old primary then needs a rebuild.
	ModeSiteLoss Mode = "SiteLoss"
)

// Phase is a phase of a run.
// +kubebuilder:validation:Enum=Hold;Failover;PromoteToGitOps;Release
type Phase string

const (
	PhaseHold            Phase = "Hold"
	PhaseFailover        Phase = "Failover"
	PhasePromoteToGitOps Phase = "PromoteToGitOps"
	PhaseRelease         Phase = "Release"
)

// Phases lists the phases in the order a run takes them.
var Phases = []Phase{PhaseHold, PhaseFailover, PhasePromoteToGitOps, PhaseRelease}

// SiteFailover is the ledger of one run: what it will do and what it did.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sfo
// +kubebuilder:printcolumn:name="Workload",type=string,JSONPath=`.spec.workload.name`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="From",type=string,JSONPath=`.spec.from`
// +kubebuilder:printcolumn:name="To",type=string,JSONPath=`.spec.to`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SiteFailover struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SiteFailoverSpec   `json:"spec"`
	Status SiteFailoverStatus `json:"status,omitempty"`
}

// SiteFailoverList is a list of SiteFailover.
// +kubebuilder:object:root=true
type SiteFailoverList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SiteFailover `json:"items"`
}

// SiteFailoverSpec is the run's request and its plan. It never changes
// after the run starts.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type SiteFailoverSpec struct {
	// Workload is the workload's configuration when the run started.
	Workload WorkloadSnapshot `json:"workload"`

	// Mode is the kind of run.
	Mode Mode `json:"mode"`

	// From is the site of the current primary.
	From string `json:"from"`

	// To is the site of the new primary.
	To string `json:"to"`

	// Steps is the plan, in the order the run takes them.
	// +kubebuilder:validation:MinItems=1
	Steps []Step `json:"steps"`
}

// WorkloadSnapshot is a copy of a Workload.
type WorkloadSnapshot struct {
	// Name is the Workload's name.
	Name string `json:"name"`
	// Spec is the Workload's spec.
	Spec WorkloadSpec `json:"spec"`
}

// StepType is the action of a step.
// +kubebuilder:validation:Enum=Hold;Reconcile;Patch;Wait;Capture;Git;VerifyGit;Release;VerifyNoop;Routing
type StepType string

const (
	// StepHold stops Flux from applying an object.
	StepHold StepType = "Hold"
	// StepReconcile asks Flux to reconcile and waits until it has.
	StepReconcile StepType = "Reconcile"
	// StepPatch changes a live object through the workload's API.
	StepPatch StepType = "Patch"
	// StepWait waits for conditions on a live object.
	StepWait StepType = "Wait"
	// StepCapture copies a value from a live object into the ledger.
	StepCapture StepType = "Capture"
	// StepGit writes the change that makes Git equal the live state.
	StepGit StepType = "Git"
	// StepVerifyGit checks that applying Git would change nothing.
	StepVerifyGit StepType = "VerifyGit"
	// StepRelease lets Flux apply an object again.
	StepRelease StepType = "Release"
	// StepVerifyNoop checks that Flux changed nothing after release.
	StepVerifyNoop StepType = "VerifyNoop"
	// StepRouting checks that routing followed the switch.
	StepRouting StepType = "Routing"
)

// Step is one action of a run, as data. The field named by Type is set.
type Step struct {
	// ID is unique within the run.
	ID string `json:"id"`

	// Phase is the phase the step belongs to.
	Phase Phase `json:"phase"`

	// Type is the step's action.
	Type StepType `json:"type"`

	// Site is the site the step acts on. Empty for a step on Git.
	// +optional
	Site string `json:"site,omitempty"`

	// Description is one line for the operator.
	Description string `json:"description"`

	// Deferrable lets the run go on while the step's site is unreachable.
	// The step then waits for the site to come back.
	// +optional
	Deferrable bool `json:"deferrable,omitempty"`

	// +optional
	Hold *ObjectStep `json:"hold,omitempty"`
	// +optional
	Reconcile *ReconcileStep `json:"reconcile,omitempty"`
	// +optional
	Patch *PatchStep `json:"patch,omitempty"`
	// +optional
	Wait *WaitStep `json:"wait,omitempty"`
	// +optional
	Capture *CaptureStep `json:"capture,omitempty"`
	// +optional
	Git *GitStep `json:"git,omitempty"`
	// +optional
	VerifyGit *VerifyStep `json:"verifyGit,omitempty"`
	// +optional
	Release *ObjectStep `json:"release,omitempty"`
	// +optional
	VerifyNoop *VerifyStep `json:"verifyNoop,omitempty"`
	// +optional
	Routing *RoutingStep `json:"routing,omitempty"`
}

// ObjectRef names a live object.
type ObjectRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
}

// ObjectStep acts on one object that Flux applies.
type ObjectStep struct {
	Object ObjectRef `json:"object"`

	// Kustomization is the Flux Kustomization that applied the object at
	// plan time. Unset when the site was unreachable. The step reads the
	// object's own labels when it runs.
	// +optional
	Kustomization *ObjectKey `json:"kustomization,omitempty"`
}

// ReconcileStep asks Flux to reconcile and waits until Flux has handled
// the request. The Kustomizations' GitRepository is reconciled first.
type ReconcileStep struct {
	// Kustomizations are the Flux Kustomizations at the step's site. Empty
	// means every Kustomization that applies an object this run holds at
	// the site.
	// +optional
	Kustomizations []ObjectKey `json:"kustomizations,omitempty"`

	// Revision, when set, is the commit Flux must have applied. It may
	// reference a captured value as $(name).
	// +optional
	Revision string `json:"revision,omitempty"`

	// Timeout bounds the wait.
	Timeout metav1.Duration `json:"timeout"`
}

// PatchStep applies a JSON merge patch to a live object.
type PatchStep struct {
	Object ObjectRef `json:"object"`

	// Merge is the JSON merge patch. A string value $(name) is replaced
	// by the captured value of that name.
	Merge string `json:"merge"`
}

// Operator compares a value.
// +kubebuilder:validation:Enum=Equals;NotEquals;NotEmpty;Empty;Contains
type Operator string

const (
	OpEquals    Operator = "Equals"
	OpNotEquals Operator = "NotEquals"
	OpNotEmpty  Operator = "NotEmpty"
	OpEmpty     Operator = "Empty"
	// OpContains holds when one of the space-separated values the
	// JSONPath selects equals Value.
	OpContains Operator = "Contains"
)

// Condition compares one field of a live object.
type Condition struct {
	// JSONPath selects the field, such as {.status.currentPrimary}.
	JSONPath string `json:"jsonPath"`

	// Operator compares the field.
	Operator Operator `json:"operator"`

	// Value is the operand of Equals and NotEquals. It may reference a
	// captured value as $(name).
	// +optional
	Value string `json:"value,omitempty"`
}

// WaitStep waits until every condition holds on a live object.
type WaitStep struct {
	Object     ObjectRef       `json:"object"`
	Conditions []Condition     `json:"conditions"`
	Timeout    metav1.Duration `json:"timeout"`
}

// CaptureStep copies one field of a live object into the ledger.
type CaptureStep struct {
	Object ObjectRef `json:"object"`

	// JSONPath selects the field.
	JSONPath string `json:"jsonPath"`

	// Into is the name the value gets in the ledger.
	Into string `json:"into"`
}

// GitStep writes one commit that makes Git equal the live state.
type GitStep struct {
	// Changes are the fields the commit sets.
	Changes []GitChange `json:"changes"`

	// Message is the commit message.
	Message string `json:"message"`
}

// GitChange sets one field in the repository.
type GitChange struct {
	// Site, Object and Field name the live field this change mirrors.
	Site   string    `json:"site"`
	Object ObjectRef `json:"object"`
	Field  string    `json:"field"`

	// File is the file, relative to the repository root. Empty until the
	// manifest is found, when Git could not be read at plan time.
	// +optional
	File string `json:"file,omitempty"`

	// Path is the field's path in the file.
	// +optional
	Path string `json:"path,omitempty"`

	// Document is the index of the YAML document in the file.
	// +optional
	Document *int32 `json:"document,omitempty"`

	// Value is the field's new value as JSON, such as "pg-b" or 2. It may
	// reference a captured value as $(name). Unset means remove the field.
	// +optional
	Value *string `json:"value,omitempty"`
}

// VerifyStep checks the held objects at the step's site against Git.
type VerifyStep struct {
	// Objects are the objects to check.
	Objects []ObjectRef `json:"objects"`

	// Fields are the fields this run changed.
	// +optional
	Fields []ObjectField `json:"fields,omitempty"`
}

// ObjectField is one field of one object.
type ObjectField struct {
	Object ObjectRef `json:"object"`
	Field  string    `json:"field"`
}

// RoutingStep waits until Strait routes the workload to the new primary.
type RoutingStep struct {
	// Service is the Strait service at the step's site.
	Service ObjectKey `json:"service"`

	// Expected is the member Strait must route to.
	Expected string `json:"expected"`

	Timeout metav1.Duration `json:"timeout"`
}

// RunState is the state of a run.
// +kubebuilder:validation:Enum=Running;Waiting;Stopped;Succeeded
type RunState string

const (
	// RunRunning means a tack process is running the plan, or was
	// interrupted while it did.
	RunRunning RunState = "Running"
	// RunWaiting means the run waits for something outside it, such as Git
	// during break-glass. tack resume continues it.
	RunWaiting RunState = "Waiting"
	// RunStopped means the run met something unexpected and waits for the
	// operator. Reason says what.
	RunStopped RunState = "Stopped"
	// RunSucceeded means every step that could run has run.
	RunSucceeded RunState = "Succeeded"
)

// Reasons a run stops or waits.
const (
	ReasonGitUnreachable      = "GitUnreachable"
	ReasonGitChanged          = "GitChanged"
	ReasonGitDiffersFromLive  = "GitDiffersFromLive"
	ReasonTokenMissing        = "TokenMissing"
	ReasonFencedSiteIsPrimary = "FencedSiteIsPrimary"
	ReasonHeldByAnotherRun    = "HeldByAnotherRun"
	ReasonFluxChanged         = "FluxChanged"
	ReasonStepFailed          = "StepFailed"
)

// StepState is the state of a step.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Deferred
type StepState string

const (
	StepPending   StepState = "Pending"
	StepRunning   StepState = "Running"
	StepSucceeded StepState = "Succeeded"
	StepFailed    StepState = "Failed"
	// StepDeferred means the step's site was unreachable and the step is
	// deferrable. It runs when the site comes back.
	StepDeferred StepState = "Deferred"
)

// SiteFailoverStatus records what the run did.
type SiteFailoverStatus struct {
	// State is the run's state.
	// +optional
	State RunState `json:"state,omitempty"`

	// Reason says why the run waits or stopped.
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message explains the state to the operator.
	// +optional
	Message string `json:"message,omitempty"`

	// Phase is the phase of the current or last step.
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// Phases records each phase's start and end.
	// +optional
	// +listType=map
	// +listMapKey=name
	Phases []PhaseStatus `json:"phases,omitempty"`

	// Steps records each step's start and end, in plan order.
	// +optional
	// +listType=map
	// +listMapKey=id
	Steps []StepStatus `json:"steps,omitempty"`

	// Captured holds the values captured for later steps, such as tokens.
	// +optional
	Captured map[string]string `json:"captured,omitempty"`

	// Held lists the objects this run holds or held.
	// +optional
	Held []HeldObject `json:"held,omitempty"`

	// Git records the run's view of the repository.
	// +optional
	Git GitStatus `json:"git,omitempty"`

	// FollowUps are actions left for the operator after the run.
	// +optional
	FollowUps []FollowUp `json:"followUps,omitempty"`

	// StartedAt is when the run started.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// FinishedAt is when the run succeeded.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
}

// PhaseStatus records one phase.
type PhaseStatus struct {
	Name Phase `json:"name"`
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
}

// StepStatus records one step.
type StepStatus struct {
	ID    string    `json:"id"`
	State StepState `json:"state"`
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	// Attempts counts the starts of the step.
	// +optional
	Attempts int32 `json:"attempts,omitempty"`
	// Message is the step's last result or error.
	// +optional
	Message string `json:"message,omitempty"`
}

// HeldObject is an object the run holds or held.
type HeldObject struct {
	Site   string    `json:"site"`
	Object ObjectRef `json:"object"`
	// Kustomization is the Flux Kustomization that applies the object.
	Kustomization ObjectKey `json:"kustomization"`
	// HeldAt is when the run held the object.
	HeldAt *metav1.Time `json:"heldAt,omitempty"`
	// ReleasedAt is when the run released it.
	// +optional
	ReleasedAt *metav1.Time `json:"releasedAt,omitempty"`
	// Generation is the object's generation at release.
	// +optional
	Generation int64 `json:"generation,omitempty"`
}

// GitStatus records the run's view of the repository.
type GitStatus struct {
	// BaseRevision is the commit live matched when the run started.
	// +optional
	BaseRevision string `json:"baseRevision,omitempty"`

	// AcceptedRevision is a later commit the operator accepted as the base.
	// +optional
	AcceptedRevision string `json:"acceptedRevision,omitempty"`

	// Commit is the run's own commit.
	// +optional
	Commit string `json:"commit,omitempty"`
}

// FollowUp is an action left for the operator.
type FollowUp struct {
	Site string `json:"site"`
	// Action is what to do, such as Rebuild.
	Action string `json:"action"`
	// Reason says why.
	Reason string `json:"reason"`
}
