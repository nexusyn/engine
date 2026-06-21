package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsPreferenceQuery_EN_Recommendation(t *testing.T) {
	cases := []string{
		"Can you recommend a podcast?",
		"What should I read this weekend?",
		"What can I do tonight?",
		"Suggest me a restaurant",
		"Any good books?",
		"I love jazz",
		"I avoid red meat",
		"What would I enjoy watching?",
		// Sprint 3.4 — phrasings the old gate missed (11/17 preference misses):
		"Any suggestions?", // plural — old \bsuggestion\b bug
		"I'm planning a trip to Denver soon. Any suggestions?",          // plural again
		"Any tips for keeping it clean?",                                // tips
		"Any tips on what to bake?",                                     // tips
		"Do you have any ideas on how I can find new inspiration?",      // ideas
		"Do you think it would be a good idea to attend my reunion?",    // do you think / good idea
		"I'm trying to decide whether to buy a NAS. What do you think?", // trying to decide / do you think
		"I was thinking about rearranging the furniture. Any tips?",     // thinking about
	}
	for _, q := range cases {
		assert.True(t, IsPreferenceQuery(q), "should match: %q", q)
	}
}

func TestIsPreferenceQuery_PTBR_Recommendation(t *testing.T) {
	cases := []string{
		"Me recomenda um podcast?",
		"O que eu devo assistir?",
		"O que posso comer hoje?",
		"Me sugere um lugar pra jantar",
		"Adoro jazz",
		"Detesto comida apimentada",
		"Prefiro mornings",
	}
	for _, q := range cases {
		assert.True(t, IsPreferenceQuery(q), "should match PT-BR: %q", q)
	}
}

func TestIsPreferenceQuery_Negatives(t *testing.T) {
	cases := []string{
		"What's the temperature today?",
		"How many years older is my grandma?",
		"Where did I park the car?",
		"Que horas são?",
		"Quando foi a última reunião?",
	}
	for _, q := range cases {
		assert.False(t, IsPreferenceQuery(q), "should NOT match: %q", q)
	}
}

func TestFormatKnownFacts_Empty(t *testing.T) {
	assert.Equal(t, "", FormatKnownFacts(nil))
	assert.Equal(t, "", FormatKnownFacts([]PreferenceFact{}))
}

func TestFormatKnownFacts_RendersBlock(t *testing.T) {
	facts := []PreferenceFact{
		{Name: "I love spicy food", Kind: "preference", Polarity: "like", Category: "food"},
		{Name: "I avoid podcasts about true crime", Kind: "preference", Polarity: "dislike", Category: "media"},
		{Name: "Always check git status before commit", Kind: "lesson"},
	}
	out := FormatKnownFacts(facts)
	assert.Contains(t, out, "KNOWN PREFERENCES")
	assert.Contains(t, out, "LIKES: I love spicy food")
	assert.Contains(t, out, "AVOID: I avoid podcasts about true crime")
	assert.Contains(t, out, "Always check git status before commit")
	assert.Contains(t, out, "category=food")
	assert.Contains(t, out, "category=media")
}
