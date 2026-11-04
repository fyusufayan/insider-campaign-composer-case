package ai

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Demo content generator: deterministic, fixed English templates for running
// the app without an API key. It is NOT a copywriter:
//   - it reads only the request document (channels, fields, content brief);
//     the audience rule is ignored;
//   - the goal selects one of three fixed storylines (cart recovery, win
//     back, generic) by keyword; the tone one of three fixed voices
//     (friendly, urgent, formal); the length the number of email paragraphs;
//   - the offer is rendered only from its validated kind and integer value,
//     so it never mentions an incentive the brief did not authorize;
//   - no text from the brief (goal or tone strings) is ever copied into the
//     output, so user input can never become template syntax.
//
// Its output goes through the same Generator pipeline (strict decode,
// content.Parse, offer policy, repair) as real model output; a repair turn
// receives the same deterministic answer.

func demoContentResponder(req llm.Request) (string, error) {
	if len(req.Messages) == 0 || req.Messages[0].Role != llm.RoleUser {
		return "", errors.New("demo: request has no content request")
	}
	raw, ok := extractContentRequest(req.Messages[0].Content)
	if !ok {
		return "", errors.New("demo: request has no content request")
	}
	chs, cb, err := parseDemoContentRequest(raw)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(demoContent(cb, chs))
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// parseDemoContentRequest strictly decodes the request document.
func parseDemoContentRequest(raw []byte) ([]segment.Channel, ContentBrief, error) {
	bad := errors.New("demo: malformed content request")
	if fe := scanJSON(raw); fe != nil {
		return nil, ContentBrief{}, bad
	}
	var errs segment.Errors
	root := object(&errs, "", raw, "channels", "fields", "content_brief", "audience_rule")
	if root == nil {
		return nil, ContentBrief{}, bad
	}
	chs := channels(&errs, "channels", root.get("channels", true))
	cb, _ := contentBrief(&errs, "content_brief", root.get("content_brief", true))
	if len(errs) > 0 {
		return nil, ContentBrief{}, bad
	}
	return chs, cb, nil
}

type demoTone int

const (
	toneFriendly demoTone = iota
	toneUrgent
	toneFormal
)

type demoGoal int

const (
	goalGeneric demoGoal = iota
	goalCart
	goalWinBack
)

func demoToneOf(tone string) demoTone {
	t := strings.ToLower(tone)
	switch {
	case strings.Contains(t, "urgent"):
		return toneUrgent
	case strings.Contains(t, "formal"), strings.Contains(t, "professional"):
		return toneFormal
	default:
		return toneFriendly
	}
}

func demoGoalOf(goal string) demoGoal {
	g := strings.ToLower(goal)
	switch {
	case strings.Contains(g, "cart"), strings.Contains(g, "checkout"):
		return goalCart
	case strings.Contains(g, "win_back"), strings.Contains(g, "winback"), strings.Contains(g, "reactivat"), strings.Contains(g, "inactive"):
		return goalWinBack
	default:
		return goalGeneric
	}
}

// demoOffer returns the offer phrase ("30% off your order") or "".
func demoOffer(o Offer) string {
	switch o.Kind {
	case OfferPercentDiscount:
		return strconv.Itoa(o.Value) + "% off your order"
	case OfferFreeShipping:
		return "free shipping on your order"
	default:
		return ""
	}
}

// demoContent builds the fields of the selected channels.
func demoContent(cb ContentBrief, chs []segment.Channel) map[string]string {
	tone, goal, offer := demoToneOf(cb.Tone), demoGoalOf(cb.Goal), demoOffer(cb.Offer)

	greeting := map[demoTone]string{
		toneFriendly: "Hi {{first_name|there}}",
		toneUrgent:   "Hi {{first_name|there}}",
		toneFormal:   "Dear {{first_name|customer}}",
	}[tone]
	core := map[demoGoal]string{
		goalCart:    "the items in your cart are still waiting for you.",
		goalWinBack: "we have missed you and picked some new favourites for you.",
		goalGeneric: "we have picked some new favourites for you.",
	}[goal]
	cta := map[demoGoal]map[demoTone]string{
		goalCart:    {toneFriendly: "Complete your order today.", toneUrgent: "Complete your order now before they are gone.", toneFormal: "You can complete your order at any time."},
		goalWinBack: {toneFriendly: "Come back and take a look.", toneUrgent: "Come back and take a look now.", toneFormal: "We would be pleased to welcome you back."},
		goalGeneric: {toneFriendly: "Take a look today.", toneUrgent: "Take a look now.", toneFormal: "We would be pleased to welcome you."},
	}[goal][tone]
	offerSentence := ""
	if offer != "" {
		offerSentence = " Enjoy " + offer + "."
	}

	out := map[string]string{}
	for _, ch := range chs {
		switch ch {
		case segment.ChannelSMS:
			// Worst case ≤ 150 runes (the prompt's SMS target) for every
			// combination; asserted by the tests.
			smsCore := map[demoGoal]string{
				goalCart:    "your cart is waiting for you.",
				goalWinBack: "we have missed you!",
				goalGeneric: "new favourites are waiting for you.",
			}[goal]
			out[content.FieldSMSBody] = greeting + ", " + smsCore + offerSentence + " " + cta
		case segment.ChannelEmail:
			subject := map[demoGoal]string{
				goalCart:    "{{first_name|Hello}}, your cart is waiting",
				goalWinBack: "{{first_name|Hello}}, we have missed you",
				goalGeneric: "{{first_name|Hello}}, new favourites for you",
			}[goal]
			switch cb.Offer.Kind {
			case OfferPercentDiscount:
				subject += ": " + strconv.Itoa(cb.Offer.Value) + "% off inside"
			case OfferFreeShipping:
				subject += ": free shipping inside"
			}
			paras := []string{
				greeting + ",",
				strings.ToUpper(core[:1]) + core[1:] + offerSentence,
			}
			if cb.Length != LengthShort {
				paras = append(paras, map[demoGoal]string{
					goalCart:    "Your selection is saved, so finishing your order only takes a moment.",
					goalWinBack: "A lot has changed since your last visit, and our latest arrivals are ready for you.",
					goalGeneric: "Our latest arrivals are ready for you to explore.",
				}[goal])
			}
			if cb.Length == LengthLong {
				paras = append(paras, "Shoppers in {{city|your area}} have been enjoying our newest picks, and we think you will love them too. If you need any help, our team is happy to assist.")
			}
			paras = append(paras, cta, map[demoTone]string{
				toneFriendly: "See you soon!",
				toneUrgent:   "Don't wait too long!",
				toneFormal:   "Kind regards,\nThe team",
			}[tone])
			out[content.FieldEmailSubject] = subject
			out[content.FieldEmailBody] = strings.Join(paras, "\n\n")
		}
	}
	return out
}
