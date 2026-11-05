package ai

import (
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Offer policy of generated content (content_v1). The content brief's offer
// is the only incentive the marketer authorized; generated copy that claims
// any other incentive is rejected (and goes through the repair turn) instead
// of silently entering a draft that the marketer might approve.
//
// This is a deterministic, conservative keyword/pattern check over English
// and common Turkish phrasings, not language understanding: it can reject
// harmless text (e.g. "100% cotton" when no discount is authorized) and it
// cannot catch every paraphrase. It complements, and never replaces, the
// marketer's review before approval. It depends on the content brief, so it
// lives with the generator; content.Parse (brief-independent) is unchanged.

// Policy codes (field errors on sms_body / email_subject / email_body).
const (
	// CodeUnauthorizedOffer: the text claims an incentive (percentage,
	// discount, coupon, free shipping, gift …) other than the authorized one.
	CodeUnauthorizedOffer = "unauthorized_offer"
	// CodeUnauthorizedClaim: the text states a monetary amount or price; the
	// content brief never contains one.
	CodeUnauthorizedClaim = "unauthorized_claim"
	// WarnOfferNotMentioned: an authorized offer is not mentioned in a
	// message (non-blocking, ARCHITECTURE §8.6).
	WarnOfferNotMentioned = "offer_not_mentioned"
)

var (
	// percentRe captures the number of "30%", "30 %", "%30" (Turkish),
	// "30 percent", "30 per cent" and "yüzde 30".
	percentRe = regexp.MustCompile(`(\d+)\s*(?:%|percent\b|per cent\b)|%\s*(\d+)|yüzde\s*(\d+)`)
	// moneyRe matches a currency amount ("₺50", "$5", "50 TL", "20 euros").
	moneyRe = regexp.MustCompile(`[₺$€£]\s*\d|\d\s*(?:[₺$€£]|(?:tl|try|usd|eur|lira|dollars?|euros?)\b)`)

	// discountTerms describe a percentage discount itself ("20% off",
	// "20% discount", "%20 indirim"); they are allowed with an authorized
	// percent_discount (matched on folded text).
	discountTerms = []string{"discount", "indirim", "percent off", "per cent off", "% off"}
	// separateIncentiveTerms are incentives independent of a percentage
	// discount and of free shipping; no offer kind authorizes them.
	separateIncentiveTerms = []string{
		"coupon", "kupon", "voucher", "promo code", "promotion code", "promosyon",
		"cashback", "cash back", "buy one get one", "bogo", "half price", "half-price", "yarı fiyat",
		"free gift", "hediye",
	}
	freeShippingTerms = []string{
		"free shipping", "free delivery", "shipping is free", "delivery is free", "shipping for free",
		"ücretsiz kargo", "bedava kargo", "kargo bedava", "ücretsiz teslimat",
	}
)

var offerFolder = strings.NewReplacer("İ", "i", "I", "i")

// foldOffer lower-cases s (Turkish dotted/dotless capital I → i) after NFC.
func foldOffer(s string) string { return strings.ToLower(offerFolder.Replace(norm.NFC.String(s))) }

func containsAny(s string, terms []string) bool {
	for _, t := range terms {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}

// percents returns the percentages stated in folded text (-1 for a value
// that does not fit an int).
func percents(s string) []int {
	var out []int
	for _, m := range percentRe.FindAllStringSubmatch(s, -1) {
		digits := m[1] + m[2] + m[3]
		n, err := strconv.Atoi(digits)
		if err != nil {
			n = -1
		}
		out = append(out, n)
	}
	return out
}

// offerField is one generated text field.
type offerField struct {
	path string
	text string
}

// checkOfferPolicy applies the offer policy to the generated fields (in
// field order). It returns blocking errors and non-blocking warnings;
// messages never echo the text.
func checkOfferPolicy(fields []offerField, o Offer) (errs segment.Errors, warns []segment.FieldError) {
	mentioned := map[string]bool{}
	for _, f := range fields {
		s := foldOffer(f.text)
		pcts := percents(s)
		free := containsAny(s, freeShippingTerms)
		discount := containsAny(s, discountTerms)
		separate := containsAny(s, separateIncentiveTerms)

		switch o.Kind {
		case OfferPercentDiscount:
			for _, p := range pcts {
				if p != o.Value {
					errs = append(errs, segment.FieldError{Path: f.path, Code: CodeUnauthorizedOffer,
						Message: "mentions a percentage other than the authorized " + strconv.Itoa(o.Value) + "% discount"})
					break
				}
			}
			if free {
				errs = append(errs, segment.FieldError{Path: f.path, Code: CodeUnauthorizedOffer,
					Message: "mentions free shipping, which the content brief does not authorize"})
			}
			if separate {
				errs = append(errs, segment.FieldError{Path: f.path, Code: CodeUnauthorizedOffer,
					Message: "mentions an incentive other than the authorized " + strconv.Itoa(o.Value) + "% discount"})
			}
			mentioned[f.path] = len(pcts) > 0
		case OfferFreeShipping:
			if len(pcts) > 0 || discount || separate {
				errs = append(errs, segment.FieldError{Path: f.path, Code: CodeUnauthorizedOffer,
					Message: "mentions a discount or incentive; the content brief authorizes only free shipping"})
			}
			mentioned[f.path] = free
		default:
			if len(pcts) > 0 || discount || separate || free {
				errs = append(errs, segment.FieldError{Path: f.path, Code: CodeUnauthorizedOffer,
					Message: "mentions a discount or incentive; the content brief authorizes no offer"})
			}
		}
		if moneyRe.MatchString(s) {
			errs = append(errs, segment.FieldError{Path: f.path, Code: CodeUnauthorizedClaim,
				Message: "states a monetary amount or price; the content brief contains none"})
		}
	}
	if o.Kind == OfferNone || len(errs) > 0 {
		return errs, nil
	}
	has := func(p string) bool {
		for _, f := range fields {
			if f.path == p {
				return true
			}
		}
		return false
	}
	if has(content.FieldSMSBody) && !mentioned[content.FieldSMSBody] {
		warns = append(warns, segment.FieldError{Path: content.FieldSMSBody, Code: WarnOfferNotMentioned,
			Message: "the SMS does not mention the authorized offer"})
	}
	if has(content.FieldEmailBody) && !mentioned[content.FieldEmailSubject] && !mentioned[content.FieldEmailBody] {
		warns = append(warns, segment.FieldError{Path: content.FieldEmailBody, Code: WarnOfferNotMentioned,
			Message: "the email does not mention the authorized offer"})
	}
	return nil, warns
}

// OfferPolicyAdvisories applies the same offer policy to validated content
// that the marketer wrote or edited, and returns every finding as a
// non-blocking warning (codes unauthorized_offer, unauthorized_claim,
// offer_not_mentioned). The policy blocks only model output: the marketer is
// the author and the approver, and the keyword check is conservative (it can
// flag harmless text such as "100% cotton"), so it informs instead of
// refusing a human edit. AI versions read back pass it without findings
// other than offer_not_mentioned, because they were blocked on generation.
func OfferPolicyAdvisories(c content.Content, o Offer) []segment.FieldError {
	var fields []offerField
	if t, ok := c.SMSBody(); ok {
		fields = append(fields, offerField{path: content.FieldSMSBody, text: t.String()})
	}
	if t, ok := c.EmailSubject(); ok {
		fields = append(fields, offerField{path: content.FieldEmailSubject, text: t.String()})
	}
	if t, ok := c.EmailBody(); ok {
		fields = append(fields, offerField{path: content.FieldEmailBody, text: t.String()})
	}
	errs, warns := checkOfferPolicy(fields, o)
	out := make([]segment.FieldError, 0, len(errs)+len(warns))
	out = append(out, errs...)
	return append(out, warns...)
}
