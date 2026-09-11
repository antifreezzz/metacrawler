package llm

import (
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

// Vectorizer реализует детерминированную in-memory векторизацию текста в Go
// на основе токенизации слов, биграмм и подсловных n-грамм с L2-нормализацией.
type Vectorizer struct {
	dim int
}

func NewVectorizer(dim int) *Vectorizer {
	if dim <= 0 {
		dim = 256
	}
	return &Vectorizer{dim: dim}
}

var defaultStopwords = map[string]bool{
	"a": true, "about": true, "above": true, "after": true, "again": true, "against": true,
	"all": true, "am": true, "an": true, "and": true, "any": true, "are": true, "aren't": true,
	"as": true, "at": true, "be": true, "because": true, "been": true, "before": true, "being": true,
	"below": true, "between": true, "both": true, "but": true, "by": true, "can't": true, "cannot": true,
	"could": true, "did": true, "do": true, "does": true, "doing": true, "down": true, "during": true,
	"each": true, "few": true, "for": true, "from": true, "further": true, "had": true, "has": true,
	"have": true, "having": true, "he": true, "her": true, "here": true, "hers": true, "herself": true,
	"him": true, "himself": true, "his": true, "how": true, "i": true, "if": true, "in": true, "into": true,
	"is": true, "isn't": true, "it": true, "its": true, "itself": true, "let's": true, "me": true,
	"more": true, "most": true, "mustn't": true, "my": true, "myself": true, "no": true, "nor": true,
	"not": true, "of": true, "off": true, "on": true, "once": true, "only": true, "or": true, "other": true,
	"ought": true, "our": true, "ours": true, "ourselves": true, "out": true, "over": true, "own": true,
	"same": true, "shan't": true, "she": true, "should": true, "so": true, "some": true, "such": true,
	"than": true, "that": true, "the": true, "their": true, "theirs": true, "them": true, "themselves": true,
	"then": true, "there": true, "these": true, "they": true, "this": true, "those": true, "through": true,
	"to": true, "too": true, "under": true, "until": true, "up": true, "very": true, "was": true,
	"wasn't": true, "we": true, "were": true, "weren't": true, "what": true, "when": true, "where": true,
	"which": true, "while": true, "who": true, "whom": true, "why": true, "with": true, "won't": true,
	"would": true, "you": true, "your": true, "yours": true, "yourself": true, "yourselves": true,
	"и": true, "в": true, "во": true, "не": true, "что": true, "он": true, "на": true, "я": true,
	"с": true, "со": true, "как": true, "а": true, "то": true, "все": true, "она": true, "так": true,
	"его": true, "но": true, "да": true, "ты": true, "к": true, "у": true, "же": true, "вы": true,
	"за": true, "бы": true, "по": true, "только": true, "ее": true, "мне": true, "было": true, "вот": true,
	"от": true, "меня": true, "еще": true, "о": true, "из": true, "ему": true, "теперь": true, "когда": true,
	"даже": true, "ну": true, "вдруг": true, "ли": true, "если": true, "уже": true, "или": true, "ни": true,
	"быть": true, "был": true, "него": true, "до": true, "вас": true, "нибудь": true, "опять": true, "уж": true,
	"вам": true, "ведь": true, "там": true, "потом": true, "себя": true, "ничего": true, "ей": true, "может": true,
	"они": true, "тут": true, "где": true, "есть": true, "надо": true, "ней": true, "для": true, "мы": true,
	"тебя": true, "их": true, "чем": true, "была": true, "сам": true, "чтоб": true, "без": true, "будто": true,
	"чего": true, "раз": true, "тоже": true, "себе": true, "под": true, "будет": true, "ж": true, "тогда": true,
	"кто": true, "этот": true, "того": true, "потому": true, "этого": true, "какой": true, "совсем": true,
	"ним": true, "здесь": true, "этом": true, "один": true, "почти": true, "мой": true, "тем": true, "чтобы": true,
	"нее": true, "сейчас": true, "были": true, "куда": true, "зачем": true, "всех": true, "никогда": true,
	"можно": true, "при": true, "наконец": true, "два": true, "об": true, "другой": true, "хоть": true,
	"после": true, "над": true, "больше": true, "тот": true, "через": true, "эти": true, "нас": true,
	"про": true, "всего": true, "них": true, "какая": true, "много": true, "разве": true, "три": true,
	"эту": true, "моя": true, "впрочем": true, "хорошо": true, "свою": true, "этой": true, "перед": true,
	"иногда": true, "лучше": true, "чуть": true, "том": true, "нельзя": true, "такой": true, "им": true,
	"более": true, "всегда": true, "конечно": true, "всю": true, "между": true,
}

// Vectorize преобразует текст в вектор заданной размерности.
func (v *Vectorizer) Vectorize(text string) []float32 {
	vec := make([]float32, v.dim)
	if strings.TrimSpace(text) == "" {
		return vec
	}

	// 1. Нормализация текста и сбор токенов
	words := extractWords(text)
	if len(words) == 0 {
		return vec
	}

	// 2. Взвешивание униграмм
	var filtered []string
	for _, w := range words {
		if defaultStopwords[w] || len(w) < 2 {
			continue
		}
		filtered = append(filtered, w)
		v.addFeature(vec, "w:"+w, 1.0)

		// Подсловные n-граммы (3-символьные) для морфологии
		runes := []rune(w)
		if len(runes) >= 4 {
			for i := 0; i <= len(runes)-3; i++ {
				ngram := string(runes[i : i+3])
				v.addFeature(vec, "sub:"+ngram, 0.35)
			}
		}
	}

	// 3. Биграммы слов для контекста (например, "action_rpg", "open_world")
	for i := 0; i < len(filtered)-1; i++ {
		bigram := filtered[i] + "_" + filtered[i+1]
		v.addFeature(vec, "bi:"+bigram, 1.5)
	}

	// 4. L2-нормализация
	var sumSq float64
	for _, val := range vec {
		sumSq += float64(val * val)
	}
	if sumSq > 0 {
		norm := float32(math.Sqrt(sumSq))
		for i := range vec {
			vec[i] /= norm
		}
	}

	return vec
}

func (v *Vectorizer) addFeature(vec []float32, feature string, weight float32) {
	h := hashFeature(feature)
	idx := int(h % uint64(v.dim))
	// Знаковый хеш (+1 или -1) для снижения коллизий (Murmur/Weinberger hash trick)
	sign := float32(1.0)
	if (h>>32)&1 == 1 {
		sign = -1.0
	}
	vec[idx] += sign * weight
}

func hashFeature(s string) uint64 {
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(s))
	return hasher.Sum64()
}

func extractWords(s string) []string {
	var words []string
	var current strings.Builder

	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(unicode.ToLower(r))
		} else {
			if current.Len() > 0 {
				words = append(words, current.String())
				current.Reset()
			}
		}
	}
	if current.Len() > 0 {
		words = append(words, current.String())
	}
	return words
}
