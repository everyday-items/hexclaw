package hub

import (
	"math"
	"strings"
	"unicode"
)

// SemanticSearch performs hybrid search: BM25 keyword + cosine similarity on text embeddings.
//
// Since embedding providers may not be available (local/offline), this uses a lightweight
// TF-IDF based approach that works without external API calls.
type SemanticSearch struct {
	idf   map[string]float64 // inverse document frequency
	docs  []searchDoc
	ready bool
}

type searchDoc struct {
	name string
	text string // combined name + description + tags for search
	tfidf map[string]float64
}

// NewSemanticSearch creates an offline semantic search index.
func NewSemanticSearch() *SemanticSearch {
	return &SemanticSearch{
		idf: make(map[string]float64),
	}
}

// Index builds the search index from skill metadata.
func (s *SemanticSearch) Index(skills []SkillMeta) {
	s.docs = make([]searchDoc, len(skills))

	// Build documents
	df := make(map[string]int) // document frequency
	for i, sk := range skills {
		text := strings.ToLower(sk.Name + " " + sk.Description + " " + strings.Join(sk.Tags, " "))
		tokens := tokenize(text)
		tf := make(map[string]float64)
		for _, t := range tokens {
			tf[t]++
		}
		// Normalize TF
		for k, v := range tf {
			tf[k] = v / float64(len(tokens))
		}
		s.docs[i] = searchDoc{name: sk.Name, text: text, tfidf: tf}
		seen := make(map[string]bool)
		for _, t := range tokens {
			if !seen[t] {
				df[t]++
				seen[t] = true
			}
		}
	}

	// Compute IDF
	n := float64(len(skills))
	s.idf = make(map[string]float64, len(df))
	for term, freq := range df {
		s.idf[term] = math.Log(1 + n/float64(freq))
	}

	// Apply IDF to TF
	for i := range s.docs {
		for term := range s.docs[i].tfidf {
			s.docs[i].tfidf[term] *= s.idf[term]
		}
	}
	s.ready = true
}

// Search returns skill names ranked by relevance to the query.
func (s *SemanticSearch) Search(query string, limit int) []string {
	if !s.ready || len(s.docs) == 0 {
		return nil
	}
	if limit <= 0 {
		limit = 10
	}

	queryTokens := tokenize(strings.ToLower(query))
	queryTF := make(map[string]float64)
	for _, t := range queryTokens {
		queryTF[t]++
	}
	for k, v := range queryTF {
		queryTF[k] = v / float64(len(queryTokens)) * s.idf[k]
	}

	type scored struct {
		name  string
		score float64
	}
	var results []scored
	for _, doc := range s.docs {
		score := cosineSimilarity(queryTF, doc.tfidf)
		if score > 0.01 {
			results = append(results, scored{doc.name, score})
		}
	}

	// Sort by score descending
	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			if results[j].score > results[i].score {
				results[i], results[j] = results[j], results[i]
			}
		}
	}

	if len(results) > limit {
		results = results[:limit]
	}

	names := make([]string, len(results))
	for i, r := range results {
		names[i] = r.name
	}
	return names
}

func tokenize(text string) []string {
	var tokens []string
	word := strings.Builder{}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			word.WriteRune(r)
		} else {
			if word.Len() > 1 { // skip single-char tokens
				tokens = append(tokens, word.String())
			}
			word.Reset()
		}
	}
	if word.Len() > 1 {
		tokens = append(tokens, word.String())
	}
	return tokens
}

func cosineSimilarity(a, b map[string]float64) float64 {
	var dot, normA, normB float64
	for k, v := range a {
		dot += v * b[k]
		normA += v * v
	}
	for _, v := range b {
		normB += v * v
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
