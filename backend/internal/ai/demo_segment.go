package ai

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Demo interpreter: a deterministic keyword matcher for running the app
// without an API key. It is NOT a natural-language understanding system:
//   - it recognizes only a fixed list of phrasings (English plus a few
//     Turkish words) for cities, gender, age, signup/inactivity windows,
//     cart/purchase events, abandoned carts, categories, channels and offers;
//   - anything else in the brief is invisible to it unless it is on the
//     known-unsupported list (VIP, site visits, opens/clicks, churn, …);
//   - city names are matched as whole words, so an English word that is
//     also a province name (e.g. "van") is taken as a city;
//   - negation and scope are handled only for the listed purchase phrases.
//
// Its one safety rule: it never emits a rule that is broader than what it
// recognized. If any recognized targeting requirement cannot be expressed
// (unsupported concept, invalid value, out-of-range window, category on an
// abandoned cart, only unsupported channels) or nothing is recognized, it
// returns an *UnsupportedTargetingError instead of a segment. Unsupported
// message-content preferences (offers) are reported in "unsupported" or
// "assumptions" and do not block the audience.

// DemoResponders returns the demo responders of the ai use cases, for
// llm.New / llm.NewDemo. Demo output goes through the same Interpreter
// pipeline (strict decode, validation, repair) as real model output.
func DemoResponders() map[string]llm.DemoResponder {
	return map[string]llm.DemoResponder{
		TaskSegmentInterpretation: demoSegmentResponder,
		TaskContentGeneration:     demoContentResponder,
	}
}

// DemoNotice is the first assumption of every demo interpretation, so demo
// output is identified as such wherever assumptions are shown.
const DemoNotice = "Demo interpreter (keyword matching, not Claude) produced this interpretation."

// Default window when a cart/purchase concept has no explicit window.
const demoDefaultWindowDays = 30

func demoSegmentResponder(req llm.Request) (string, error) {
	// The brief is in the first user message (repair turns repeat it there).
	if len(req.Messages) == 0 || req.Messages[0].Role != llm.RoleUser {
		return "", errors.New("demo: request has no brief")
	}
	brief, ok := extractBrief(req.Messages[0].Content)
	if !ok {
		return "", errors.New("demo: request has no brief")
	}
	env, err := demoInterpret(brief)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// demoEnvelope is the wire shape the demo emits (the schema's shape).
type demoEnvelope struct {
	Segment           json.RawMessage `json:"segment"`
	Interpretation    string          `json:"interpretation"`
	Assumptions       []string        `json:"assumptions"`
	Unsupported       []string        `json:"unsupported"`
	SuggestedChannels []string        `json:"suggested_channels"`
	ContentBrief      ContentBrief    `json:"content_brief"`
}

// demoState accumulates the interpretation of one brief.
type demoState struct {
	text        string // folded brief, single-spaced
	padded      string // " " + words + " " for whole-word matching
	conds       []segment.Condition
	phrases     []string
	assumptions []string
	unsupported []string
	// problems are targeting requirements that could not be expressed; any
	// problem rejects the whole brief.
	problems segment.Errors
}

// reject records a targeting requirement that cannot be expressed. msg must
// be a fixed text (plus parsed integers at most), never brief content.
func (s *demoState) reject(code, msg string) {
	if !s.hasProblem(msg) && len(s.problems) < MaxNotes {
		s.problems = append(s.problems, segment.FieldError{Path: "brief", Code: code, Message: msg})
	}
}

var (
	reNonWord = regexp.MustCompile(`[^\p{L}\p{N}%+$€₺]+`)
	reWindow  = regexp.MustCompile(`\b(?:last|past|previous|son|within(?: the)?(?: last| past)?)\s+(\d{1,4})\s*(days?|gun|weeks?|hafta|months?|ay|years?|yil)\b`)
	// reWindowAny detects an explicit numeric window reWindow cannot parse.
	reWindowAny = regexp.MustCompile(`\b(?:last|past|previous|son|within)\s+\d+`)
	reWindowOne = regexp.MustCompile(`\b(?:last|past)\s+(week|month|year)\b`)
	rePercent   = regexp.MustCompile(`(\d{1,4})\s*%|%\s*(\d{1,4})|(\d{1,4})\s*percent|yuzde\s*(\d{1,4})`)
	reAmount    = regexp.MustCompile(`\d+\s*(?:tl|try|lira|usd|eur|dollars?|euros?)\b|[$€₺]\s*\d+|\d+\s*[$€₺]`)
	reAgeRange  = regexp.MustCompile(`\b(?:aged?|ages|between)\s+(\d{1,3})\s*(?:-|to|and)\s*(\d{1,3})\b`)
	reAgeOver   = regexp.MustCompile(`\b(?:aged?|ages)\s+(?:over|above|at least)\s+(\d{1,3})\b|\b(?:over|older than|above)\s+(\d{1,3})\s*(?:years|yrs|yas)\b|\baged?\s+(\d{1,3})\s*\+`)
	reAgeUnder  = regexp.MustCompile(`\b(?:aged?|ages)\s+(?:under|below)\s+(\d{1,3})\b|\b(?:under|younger than|below)\s+(\d{1,3})\s*(?:years|yrs|yas)\b`)
	reAtLeast   = regexp.MustCompile(`\b(?:at least|minimum of)\s+(\d{1,3})\b|\b(\d{1,3})\s+or more\b|\b(\d{1,3})\s*\+\s*(?:purchases|orders|times)\b`)
	reInactive  = regexp.MustCompile(`\b(?:inactive|not active|not been active|haven't been active|have not been active)\s+(?:for\s+|in\s+)?(?:the\s+)?(?:last\s+|past\s+)?(\d{1,4})\s*days?\b`)
	reSignup    = regexp.MustCompile(`\b(?:signed up|registered|joined)\s+(?:in\s+|within\s+)?(?:the\s+)?(?:last|past)\s+(\d{1,4})\s*days?\b`)
)

func newDemoState(brief string) *demoState {
	t := segment.FoldKey(strings.NewReplacer("’", "'", "‘", "'").Replace(brief))
	// Words split at apostrophes ("istanbul'da" → "istanbul da"); the
	// substring text drops them ("didn't" → "didnt").
	words := strings.Fields(reNonWord.ReplaceAllString(t, " "))
	return &demoState{text: strings.ReplaceAll(t, "'", ""), padded: " " + strings.Join(words, " ") + " "}
}

// word reports whether one of the phrases occurs as whole words (apostrophes
// removed, punctuation ignored).
func (s *demoState) word(phrases ...string) bool {
	for _, p := range phrases {
		if strings.Contains(s.padded, " "+p+" ") {
			return true
		}
	}
	return false
}

// sub reports whether one of the substrings occurs in the folded brief.
func (s *demoState) sub(subs ...string) bool {
	for _, p := range subs {
		if strings.Contains(s.text, p) {
			return true
		}
	}
	return false
}

// add appends a condition after checking it alone against the validator; an
// invalid condition rejects the brief (it is never dropped or adjusted).
func (s *demoState) add(c segment.Condition, phrase, invalidMsg string) {
	if _, err := segment.Normalize(segment.Rule{Match: segment.MatchAll, Conditions: []segment.Condition{c}}); err != nil {
		s.reject(CodeInvalidTargeting, invalidMsg)
		return
	}
	s.conds = append(s.conds, c)
	s.phrases = append(s.phrases, phrase)
}

func demoInterpret(brief string) (demoEnvelope, error) {
	s := newDemoState(brief)
	s.assumptions = append(s.assumptions, DemoNotice)

	s.cities()
	s.gender()
	s.age()
	s.activity()
	goal := s.events()
	chs := s.channels()
	offer := s.offer()
	s.unsupportedConcepts()

	if len(s.conds) == 0 && len(s.problems) == 0 {
		s.reject(CodeNoTargeting, "No supported targeting criterion was recognized. Describe the audience with supported criteria such as city, age, gender, signup or inactivity days, cart additions, purchases or abandoned carts; an all-users audience is not supported.")
	}
	if len(s.problems) > 0 {
		return demoEnvelope{}, &UnsupportedTargetingError{Problems: s.problems}
	}
	if goal == "" {
		goal = "engagement"
		if offer.Kind != OfferNone {
			goal = "promotion"
		}
	}

	rule := segment.Rule{Match: segment.MatchAll, Conditions: s.conds}
	raw, err := json.Marshal(rule)
	if err != nil { // the rule types always encode
		raw = []byte(`{}`)
	}
	return demoEnvelope{
		Segment:           raw,
		Interpretation:    s.summary(),
		Assumptions:       limitNotes(s.assumptions),
		Unsupported:       limitNotes(s.unsupported),
		SuggestedChannels: chs,
		ContentBrief:      ContentBrief{Goal: goal, Tone: s.tone(), Length: s.length(chs), Offer: offer},
	}, nil
}

// summary describes the rule in one sentence.
func (s *demoState) summary() string {
	return "Users " + strings.Join(s.phrases, ", ") + "."
}

func (s *demoState) cities() {
	var names []string
	for _, c := range segment.Cities().All() {
		if s.word(c.Key) {
			names = append(names, c.Name)
		}
	}
	switch len(names) {
	case 0:
	case 1:
		s.add(segment.AttributeCondition{Field: segment.FieldCity, Op: segment.OpEq, Value: segment.StringValue(names[0])},
			"in "+names[0], "The city filter could not be applied.")
	default:
		s.add(segment.AttributeCondition{Field: segment.FieldCity, Op: segment.OpIn, Value: segment.StringsValue(names...)},
			"in "+strings.Join(names, " or "), "The city filter could not be applied.")
		s.assumptions = append(s.assumptions, "Several cities were named; users in any of them are included.")
	}
}

func (s *demoState) gender() {
	female := s.word("women", "woman", "female", "females", "ladies", "kadin", "kadinlar")
	male := s.word("men", "man", "male", "males", "erkek", "erkekler")
	switch {
	case female && male:
		s.assumptions = append(s.assumptions, "Both women and men were mentioned, so no gender filter was applied.")
	case female:
		s.add(segment.AttributeCondition{Field: segment.FieldGender, Op: segment.OpEq, Value: segment.StringValue("female")}, "with gender female", "")
	case male:
		s.add(segment.AttributeCondition{Field: segment.FieldGender, Op: segment.OpEq, Value: segment.StringValue("male")}, "with gender male", "")
	}
}

func (s *demoState) age() {
	const note = "The requested age is outside the supported range of 13–120 years."
	defer func() {
		// An age requirement that was mentioned but not understood must not
		// be dropped.
		mentioned := s.sub("aged", "years old", "year old", "age group", "age range") || s.word("age", "ages", "yas", "yasinda", "yasindaki")
		if mentioned && !s.hasField(segment.FieldAge) && !s.hasProblem(note) {
			s.reject(CodeUnsupportedTargeting, "An age requirement was mentioned but could not be understood. Use forms such as \"aged 25-40\", \"aged over 30\" or \"under 25 years\".")
		}
	}()
	if idx := reAgeRange.FindStringSubmatchIndex(s.text); idx != nil && !strings.HasPrefix(strings.TrimSpace(s.text[idx[1]:]), "day") {
		m := []string{s.text[idx[0]:idx[1]], s.text[idx[2]:idx[3]], s.text[idx[4]:idx[5]]}
		lo, hi := atoi(m[1]), atoi(m[2])
		s.add(segment.AttributeCondition{Field: segment.FieldAge, Op: segment.OpBetween, Value: segment.RangeValue(lo, hi)},
			"aged "+m[1]+" to "+m[2], note)
		return
	}
	if m := reAgeOver.FindStringSubmatch(s.text); m != nil {
		n := firstNonEmpty(m[1:]...)
		s.add(segment.AttributeCondition{Field: segment.FieldAge, Op: segment.OpGte, Value: segment.IntValue(atoi(n))},
			"aged "+n+" or older", note)
		s.assumptions = append(s.assumptions, "The age bound "+n+" is treated as inclusive.")
		return
	}
	if m := reAgeUnder.FindStringSubmatch(s.text); m != nil {
		n := firstNonEmpty(m[1:]...)
		s.add(segment.AttributeCondition{Field: segment.FieldAge, Op: segment.OpLte, Value: segment.IntValue(atoi(n))},
			"aged "+n+" or younger", note)
		s.assumptions = append(s.assumptions, "The age bound "+n+" is treated as inclusive.")
	}
}

func (s *demoState) activity() {
	if m := reInactive.FindStringSubmatch(s.text); m != nil {
		s.add(segment.AttributeCondition{Field: segment.FieldLastActiveDaysAgo, Op: segment.OpGte, Value: segment.IntValue(atoi(m[1]))},
			"inactive for at least "+m[1]+" days", "The inactivity period is outside the supported range of 0–3650 days.")
	}
	if m := reSignup.FindStringSubmatch(s.text); m != nil {
		s.add(segment.AttributeCondition{Field: segment.FieldSignupDaysAgo, Op: segment.OpLte, Value: segment.IntValue(atoi(m[1]))},
			"who signed up in the last "+m[1]+" days", "The signup period is outside the supported range of 0–3650 days.")
	}
}

// window returns the event window in days and a phrase. An explicit window
// outside 1–365 days is rejected (never capped or replaced); only an absent
// window defaults to 30 days, recorded as an assumption. ok is false when the
// window was rejected.
func (s *demoState) window() (days int, phrase string, ok bool) {
	if m := reWindow.FindStringSubmatch(s.text); m != nil {
		n := atoi(m[1])
		switch {
		case strings.HasPrefix(m[2], "week"), m[2] == "hafta":
			n *= 7
		case strings.HasPrefix(m[2], "month"), m[2] == "ay":
			n *= 30
			s.assumptions = append(s.assumptions, "A month is counted as 30 days.")
		case strings.HasPrefix(m[2], "year"), m[2] == "yil":
			n *= 365
		}
		if n < segment.MinWithinDays || n > segment.MaxWithinDays {
			s.reject(CodeInvalidTargeting, "The time window must be between 1 and 365 days (requested: "+strconv.Itoa(n)+" days).")
			return 0, "", false
		}
		return n, "in the last " + strconv.Itoa(n) + " days", true
	}
	if reWindowAny.MatchString(s.text) {
		s.reject(CodeUnsupportedTargeting, "The time window could not be understood. Use days, weeks, months or years, e.g. \"in the last 30 days\".")
		return 0, "", false
	}
	if m := reWindowOne.FindStringSubmatch(s.text); m != nil {
		switch m[1] {
		case "week":
			return 7, "in the last 7 days", true
		case "year":
			return 365, "in the last 365 days", true
		}
		s.assumptions = append(s.assumptions, "A month is counted as 30 days.")
		return 30, "in the last 30 days", true
	}
	s.assumptions = append(s.assumptions, "No time window was given; the last 30 days are used.")
	return demoDefaultWindowDays, "in the last 30 days", true
}

var demoCategorySynonyms = map[string][]string{
	segment.CategoryBeauty:      {"beauty", "cosmetics", "makeup", "kozmetik"},
	segment.CategoryBooks:       {"books", "book", "kitap"},
	segment.CategoryElectronics: {"electronics", "electronic", "elektronik", "gadgets"},
	segment.CategoryFashion:     {"fashion", "clothing", "clothes", "apparel", "giyim", "moda"},
	segment.CategoryGrocery:     {"grocery", "groceries"},
	segment.CategoryHome:        {"home goods", "home decor", "home products", "furniture"},
	segment.CategorySports:      {"sports", "sport", "spor", "fitness"},
	segment.CategoryToys:        {"toys", "toy", "oyuncak"},
}

func (s *demoState) categories() []string {
	var out []string
	for _, c := range segment.Categories() {
		if s.word(demoCategorySynonyms[c]...) {
			out = append(out, c)
		}
	}
	return out
}

// events recognises cart and purchase concepts and returns the content goal.
func (s *demoState) events() string {
	cart := s.sub("cart", "basket", "sepet")
	notPurchased := s.sub("did not complete", "didnt complete", "did not purchase", "didnt purchase", "not purchased",
		"no purchase", "without purchas", "without buying", "without ordering", "havent purchased", "have not purchased",
		"hasnt purchased", "has not purchased", "did not buy", "didnt buy", "not bought", "never purchased", "never bought",
		"never ordered", "abandon", "satin almayan", "almadi", "tamamlamayan", "terk")
	purchased := s.sub("purchased", "bought", "buyers", "made a purchase", "ordered", "customers who buy", "satin alan", "purchase")
	if !cart && !purchased && !notPurchased {
		return ""
	}
	cats := s.categories()
	days, wphrase, ok := s.window()
	if !ok {
		return ""
	}
	catPhrase := ""
	if len(cats) > 0 {
		catPhrase = " (" + strings.Join(cats, ", ") + ")"
	}

	switch {
	case cart && notPurchased:
		s.add(segment.BehaviorCondition{Predicate: segment.PredicateAbandonedCart, WithinDays: days},
			"who added a product to their cart "+wphrase+" and have not purchased since", "The abandoned-cart window is outside the supported range.")
		s.assumptions = append(s.assumptions, "'Did not complete a purchase' means no purchase after the cart addition.")
		if len(cats) > 0 {
			s.reject(CodeUnsupportedTargeting, "Abandoned carts cannot be filtered by product category. Remove the category, or target cart additions in that category instead.")
		}
		return "cart_recovery"
	case cart:
		s.add(segment.EventCondition{Event: segment.EventCartAdd, Op: segment.EventExists, WithinDays: days, CategoryIn: nilIfEmpty(cats)},
			"who added a product"+catPhrase+" to their cart "+wphrase, "The cart window is outside the supported range.")
		return "cart_recovery"
	case notPurchased:
		s.add(segment.EventCondition{Event: segment.EventPurchase, Op: segment.EventNotExists, WithinDays: days, CategoryIn: nilIfEmpty(cats)},
			"with no purchase"+catPhrase+" "+wphrase, "The purchase window is outside the supported range.")
		return "win_back"
	default:
		if m := reAtLeast.FindStringSubmatch(s.text); m != nil {
			n := atoi(firstNonEmpty(m[1:]...))
			cnt := n
			s.add(segment.EventCondition{Event: segment.EventPurchase, Op: segment.EventCountGte, Count: &cnt, WithinDays: days, CategoryIn: nilIfEmpty(cats)},
				"with at least "+strconv.Itoa(n)+" purchases"+catPhrase+" "+wphrase, "The purchase count must be between 0 and 100.")
			return "loyalty"
		}
		s.add(segment.EventCondition{Event: segment.EventPurchase, Op: segment.EventExists, WithinDays: days, CategoryIn: nilIfEmpty(cats)},
			"who made a purchase"+catPhrase+" "+wphrase, "The purchase window is outside the supported range.")
		return "loyalty"
	}
}

func (s *demoState) channels() []string {
	sms := s.word("sms", "text", "text message", "text messages", "texts", "mesaj")
	email := s.word("email", "emails", "e mail", "mail", "eposta", "e posta", "newsletter")
	other := false
	for _, o := range []struct {
		words []string
		name  string
	}{
		{[]string{"whatsapp"}, "WhatsApp"},
		{[]string{"push", "push notification", "push notifications"}, "Push notifications"},
	} {
		if s.word(o.words...) {
			other = true
			s.unsupported = append(s.unsupported, o.name+" is not a supported channel; only SMS and email are available.")
		}
	}
	if other && !sms && !email {
		// Never substitute SMS/email for an explicitly requested channel.
		s.reject(CodeUnsupportedChannel, "Only SMS and email are supported channels; the requested channel is not available. Name SMS or email in the brief.")
		return nil
	}
	switch {
	case sms && !email:
		return []string{string(segment.ChannelSMS)}
	case email && !sms:
		return []string{string(segment.ChannelEmail)}
	case !sms && !email:
		s.assumptions = append(s.assumptions, "No channel was named, so both SMS and email are suggested.")
	}
	return []string{string(segment.ChannelSMS), string(segment.ChannelEmail)}
}

func (s *demoState) offer() Offer {
	freeShipping := s.sub("free shipping", "free delivery", "ucretsiz kargo", "kargo bedava")
	if m := rePercent.FindStringSubmatch(s.text); m != nil {
		v := atoi(firstNonEmpty(m[1:]...))
		if v < MinPercentDiscount || v > MaxPercentDiscount {
			s.unsupported = append(s.unsupported, "A "+strconv.Itoa(v)+"% discount is outside the supported 1–100% range; no offer was set.")
			return Offer{Kind: OfferNone}
		}
		if freeShipping {
			s.unsupported = append(s.unsupported, "Only one offer is supported; free shipping was not included.")
		}
		return Offer{Kind: OfferPercentDiscount, Value: v}
	}
	if freeShipping {
		return Offer{Kind: OfferFreeShipping}
	}
	if reAmount.MatchString(s.text) && s.sub("off", "discount", "indirim") {
		s.assumptions = append(s.assumptions, "Fixed-amount discounts are not supported; no offer was set.")
	}
	return Offer{Kind: OfferNone}
}

// demoUnsupportedTargeting lists audience concepts with no DSL equivalent.
// Dropping them would broaden the audience, so each one rejects the brief.
var demoUnsupportedTargeting = []struct {
	subs []string
	msg  string
}{
	{[]string{"birthday", "dogum gunu"}, "Birthdays are not tracked, so a birthday audience cannot be targeted."},
	{[]string{"loyalty program", "loyalty member", "vip", "premium member"}, "Loyalty or VIP membership is not tracked, so it cannot be targeted."},
	{[]string{"opened", "clicked", "click", "newsletter open"}, "Message opens and clicks are not tracked, so they cannot be targeted."},
	{[]string{"visited", "viewed", "browsed", "page view", "website visit", "site visit"}, "Site visits and product views are not tracked, so they cannot be targeted."},
	{[]string{"spent more", "spent over", "lifetime value", "total spend", "order value"}, "Spending amounts are not supported as a targeting criterion."},
	{[]string{"wishlist", "favorites", "favourites"}, "Wishlists are not tracked, so they cannot be targeted."},
	{[]string{"app users", "installed the app", "mobile app"}, "App installs are not tracked, so they cannot be targeted."},
	{[]string{"churn", "likely to", "predicted", "propensity"}, "Predicted behaviour (such as churn likelihood) is not supported as a targeting criterion."},
}

func (s *demoState) unsupportedConcepts() {
	for _, u := range demoUnsupportedTargeting {
		if s.sub(u.subs...) {
			s.reject(CodeUnsupportedTargeting, u.msg)
		}
	}
}

// hasProblem reports whether msg was already rejected.
func (s *demoState) hasProblem(msg string) bool {
	for _, p := range s.problems {
		if p.Message == msg {
			return true
		}
	}
	return false
}

// hasField reports whether an attribute condition on f was recognized.
func (s *demoState) hasField(f segment.Field) bool {
	for _, c := range s.conds {
		if a, ok := c.(segment.AttributeCondition); ok && a.Field == f {
			return true
		}
	}
	return false
}

func (s *demoState) tone() string {
	switch {
	case s.word("urgent", "hurry", "last chance", "acele", "son sans"):
		return "urgent"
	case s.word("formal", "professional", "resmi"):
		return "formal"
	case s.word("playful", "fun", "funny", "eglenceli"):
		return "playful"
	default:
		return "friendly"
	}
}

func (s *demoState) length(chs []string) Length {
	switch {
	case s.word("long", "detailed", "uzun"):
		return LengthLong
	case s.word("short", "brief", "kisa", "reminder"):
		return LengthShort
	case len(chs) == 1 && chs[0] == string(segment.ChannelEmail):
		return LengthMedium
	default:
		return LengthShort
	}
}

func limitNotes(n []string) []string {
	out := make([]string, 0, len(n))
	seen := map[string]bool{}
	for _, s := range n {
		if s == "" || seen[s] || len(out) == MaxNotes {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
