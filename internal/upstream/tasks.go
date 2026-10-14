package upstream

import (
	"context"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
)

// Credits is a detailed credit observation for one account.
//
// Total is the size of the grant and is for display only. Expiring counts the
// part of Remain that lapses inside the requested window, and EarliestAt is
// when the next batch lapses (zero when nothing lapses).
type Credits struct {
	Remain            int64
	Total             int64
	Expiring          int64
	EarliestAt        time.Time
	EarliestRemaining int64
	// Unlimited marks an enterprise account whose grant is not metered.
	Unlimited bool
}

// CheckinResult reports the outcome of a daily check-in. A repeated check-in
// is not an error: the upstream rejects it idempotently and Already is set.
type CheckinResult struct {
	Already bool
	Credit  int64
	Energy  int64
}

// Task is one growth task as reported by the upstream. The JSON tags align the
// console API with the rest of the panel's snake_case surface.
type Task struct {
	Code         string `json:"code"`
	Title        string `json:"title,omitempty"`
	Description  string `json:"description,omitempty"`
	Desc         string `json:"desc,omitempty"`
	Credit       int64  `json:"credit,omitempty"`
	Energy       int64  `json:"energy,omitempty"`
	Target       int64  `json:"target,omitempty"`
	Current      int64  `json:"current,omitempty"`
	AcceptStatus string `json:"accept_status,omitempty"`
	Status       string `json:"status,omitempty"`
	Locked       bool   `json:"locked,omitempty"`
	JumpURL      string `json:"jump_url,omitempty"`
	Claimable    bool   `json:"claimable,omitempty"`
	Claimed      bool   `json:"claimed,omitempty"`
	// MiniProgram marks a task that is only listed and actionable when the
	// request carries the mini-program platform marker.
	MiniProgram bool `json:"mini_program,omitempty"`
}

// StreakTier is one redeemable consecutive-sign-in tier.
type StreakTier struct {
	Tier    string
	Days    int
	Credit  int64
	Energy  int64
	Cards   int
	Chances int
	Status  string
}

// TravelState is the companion-travel state machine snapshot.
type TravelState struct {
	State             string
	DailyLimitReached bool
	RecordID          int64
	RewardCredit      int64
}

// Buddy is the adopted companion, if any.
type Buddy struct {
	ID   int64
	Name string
}

// GrowthState aggregates one account's consecutive-sign-in and lottery state.
type GrowthState struct {
	StreakDays     int
	StreakMonth    int
	NextTier       string
	NextTierRemain int
	MakeupCards    int
	MakeupMax      int
	Tiers          []StreakTier
	LotteryChances int
	LotteryEnabled bool
}

// TravelStatus is the companion and travel state of one account. Buddy is nil
// when no companion has been adopted yet.
type TravelStatus struct {
	Buddy *Buddy
	State TravelState
}

// TaskScope reports which task families apply to one account. Gating is
// source-specific (for example, an international WorkBuddy account has no
// personal check-in program, and an enterprise account has none of the growth
// features), so adapters answer it rather than the scheduler guessing.
type TaskScope struct {
	Checkin  bool
	Activity bool
	Balance  bool
	Growth   bool
	Travel   bool
	Night    bool
}

// Tasks is implemented by adapters that support scheduled account maintenance
// and growth tasks. Adapters that only serve chat simply do not implement it;
// the scheduler then skips them.
//
// Every method must be safe to call repeatedly: the scheduler retries, catches
// up after downtime and lets the console trigger tasks at any time.
type Tasks interface {
	// Scope reports which task families apply to acc.
	Scope(acc *account.Account) TaskScope

	// Checkin performs the daily check-in.
	Checkin(ctx context.Context, acc *account.Account) (*CheckinResult, error)

	// Credits reports the account's credit balance and expiry picture. The
	// expiringWithin window is applied upstream-side where possible.
	Credits(ctx context.Context, acc *account.Account, expiringWithin time.Duration) (*Credits, error)

	// ReportActivity sends one conversation-activity event, which lights the
	// consecutive-sign-in day and unlocks dependent tasks.
	ReportActivity(ctx context.Context, acc *account.Account, modelID, modelName string) error

	// Growth returns the streak and lottery snapshot.
	Growth(ctx context.Context, acc *account.Account) (*GrowthState, error)

	// Travel returns the companion and travel state.
	Travel(ctx context.Context, acc *account.Account) (*TravelStatus, error)

	// ListTasks returns the growth task catalog. When miniProgram is true the
	// mini-program-only tasks are included.
	ListTasks(ctx context.Context, acc *account.Account, miniProgram bool) ([]Task, error)

	// AcceptTasks enrolls the given task codes. It is idempotent.
	AcceptTasks(ctx context.Context, acc *account.Account, codes []string, miniProgram bool) error

	// ClaimReward collects the reward of one completed task.
	ClaimReward(ctx context.Context, acc *account.Account, code string, miniProgram bool) (credit, energy int64, err error)

	// RedeemStreak claims one consecutive-sign-in tier ("7d", "14d", "28d").
	RedeemStreak(ctx context.Context, acc *account.Account, tier string) error

	// DrawLottery spends one lottery chance and reports what remains.
	DrawLottery(ctx context.Context, acc *account.Account) (remaining int, err error)

	// TravelDepart sends the companion travelling.
	TravelDepart(ctx context.Context, acc *account.Account, locationID int) error

	// TravelClaim collects a finished trip's reward.
	TravelClaim(ctx context.Context, acc *account.Account, recordID int64) (credit int64, err error)

	// BuddyAdopt accepts the companion agreement and adopts the first one.
	BuddyAdopt(ctx context.Context, acc *account.Account) error

	// NightNeed reports how many late-night conversations are still required.
	NightNeed(ctx context.Context, acc *account.Account) (int64, error)

	// NightRun performs n late-night conversations and their event chain.
	NightRun(ctx context.Context, acc *account.Account, n int) error

	// ClaimGift collects a one-off gift or compensation payout. A missing
	// payout is reported as an error by the upstream and is not fatal.
	ClaimGift(ctx context.Context, acc *account.Account) (credit int64, err error)

	// MissedYesterday reports whether yesterday's check-in is missing from the
	// activity heatmap.
	MissedYesterday(ctx context.Context, acc *account.Account) (bool, error)

	// UseMakeupCard retroactively signs in for date (YYYY-MM-DD), preserving
	// the consecutive-day streak.
	UseMakeupCard(ctx context.Context, acc *account.Account, date string) error

	// BuddyAgreement accepts the companion terms. It is idempotent and is a
	// prerequisite for adopting the first companion.
	BuddyAgreement(ctx context.Context, acc *account.Account) error

	// BuddyFirst adopts the first companion. The upstream rejects the call
	// until the activity threshold that gates adoption has been met, so a
	// failure here is expected and retried on a later pass.
	BuddyFirst(ctx context.Context, acc *account.Account) error

	// ReportDesktopEvents sends a batch of desktop-client telemetry, which is
	// the evidence the "use the desktop client" tasks are keyed on. Build the
	// payloads with the Desktop* builders in this package.
	ReportDesktopEvents(ctx context.Context, acc *account.Account, events ...map[string]any) error

	// ReportMiniProgramEvents sends a batch of mini-program telemetry, the
	// evidence the mini-program-only tasks are keyed on. Build the payloads
	// with the Mini* builders in this package.
	ReportMiniProgramEvents(ctx context.Context, acc *account.Account, events ...map[string]any) error

	// ReportWebEvent sends one browser page-behaviour event, the evidence for
	// the "visited a page" tasks.
	ReportWebEvent(ctx context.Context, acc *account.Account, eventCode, pageURL, elementID, elementName string) error

	// MarketExperts lists the real experts of the marketplace. expertType
	// selects single experts ("agent"), expert teams ("team") or both ("").
	// The ids are required verbatim by the expert-usage evidence.
	MarketExperts(ctx context.Context, acc *account.Account, expertType string) ([]MarketExpert, error)

	// DesktopChat runs one real desktop conversation, optionally addressed to
	// an expert, and returns the ids the telemetry evidence has to carry. The
	// upstream ignores evidence that quotes a locally invented request id, so
	// a real conversation has to run first. An empty modelID uses the client's
	// default fast model.
	DesktopChat(ctx context.Context, acc *account.Account, expertID, modelID string) (conversationID, requestID string, err error)

	// SetAppearanceTheme applies an appearance theme. Pair it with
	// AppearanceAppliedEvent to produce the evidence the task is keyed on.
	SetAppearanceTheme(ctx context.Context, acc *account.Account, resourceKey string) error
}

// TasksFor returns the task adapter for a source, if it supports tasks.
func TasksFor(s Source) (Tasks, bool) {
	u, ok := Get(s)
	if !ok {
		return nil, false
	}
	t, ok := u.(Tasks)
	return t, ok
}
