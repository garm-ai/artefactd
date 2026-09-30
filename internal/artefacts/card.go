package artefacts

import (
	"encoding/base64"
	"fmt"

	"github.com/garm-ai/contracts/cards"
	cardv1 "github.com/garm-ai/contracts/garm/card/v1"
	toolv1 "github.com/garm-ai/contracts/garm/tool/v1"
)

// The fact names on a release card. They are the contract with whoever reads
// one, so they are constants here and not literals at the call site.
const (
	FactURL       = "url"
	FactExpiresAt = "expires_at"
	FactCodec     = "codec"
	FactDigest    = "plaintext_sha256"
	FactDataKey   = "data_key"
)

// ReleaseCard is what read_url actually returns, and the reason it returns a
// card at all.
//
// A card is the one place in this platform where policy travels on the VALUE
// rather than on the descriptor. A field policy is decided at compile time and
// says what EVERY caller of an RPC may see of a field; an artefact store needs
// the opposite — the same field, readable by one viewer and not the next,
// decided per artefact. So the URL and the key are facts of a card labelled at
// this artefact's classification, and the daemon's step-8 walk over the card's
// value drops what the viewer does not reach, clearing the whole card and
// leaving the caller an id and nothing else.
//
// Every label is JOINED with the endpoint's own, which is what keeps a card
// valid: an element labelled BELOW the endpoint's policy fails the whole card
// with card_invalid rather than being served, and cards.Join takes the tighter
// of the two. So an artefact classified below this tool's floor is released at
// the floor, and one classified above it is released at its own classification.
func ReleaseCard(parent *toolv1.ToolPolicy, r Released) *cardv1.Card {
	access := cards.Join(
		cards.EndpointLabel(parent),
		cards.Label(r.Label.Clearance, r.Label.Compartments),
	)

	facts := []*cardv1.Fact{
		{Label: "Link", Value: r.URL, Field: FactURL, Access: access},
		{Label: "Expires", Value: r.ExpiresAt.UTC().Format(rfc3339), Field: FactExpiresAt, Access: access},
		{Label: "Codec", Value: r.Artefact.Codec, Field: FactCodec, Access: access},
		{Label: "Digest", Value: r.Artefact.PlaintextSHA256, Field: FactDigest, Access: access},
	}
	if len(r.DataKey) > 0 {
		// Base64 because a card's facts are text and a card crosses the wire as
		// protojson. It is one artefact's key, released by the one call that
		// already authorised the caller and already wrote the row.
		facts = append(facts, &cardv1.Fact{
			Label: "Key", Value: base64.StdEncoding.EncodeToString(r.DataKey),
			Field: FactDataKey, Access: access,
		})
	}

	title := r.Artefact.Label
	if title == "" {
		title = r.Artefact.MediaType
	}
	return &cardv1.Card{
		// KIND_UNSPECIFIED: the kinds the vocabulary has are START, TASK, RUN
		// and ASK, and an artefact is none of them. The daemon does not read
		// kind — it cannot tell a task card from a start card and does not need
		// to — so an honest zero is better than borrowing a kind that means
		// something else to a renderer.
		SubjectId: r.Artefact.ID,
		Title:     title,
		Body: []*cardv1.Element{
			{
				Of: &cardv1.Element_Text{Text: &cardv1.Text{
					Text: fmt.Sprintf("%s, %s. The link expires at %s and is a credential: "+
						"anyone holding it can read this artefact until then.",
						r.Artefact.MediaType, humanBytes(r.Artefact.PlaintextSizeBytes),
						r.ExpiresAt.UTC().Format(rfc3339)),
					Emphasis: cardv1.Emphasis_SUBTLE,
				}},
				Access: access,
			},
			{
				Of:     &cardv1.Element_Facts{Facts: &cardv1.FactSet{Facts: facts}},
				Access: access,
			},
		},
		Access: access,
	}
}

const rfc3339 = "2006-01-02T15:04:05Z"

// humanBytes is for a person reading a card, and it is deliberately coarse: the
// exact size is a fact of describe, and a card is a sentence.
func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d bytes", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.0f KB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1024*1024*1024))
	}
}
