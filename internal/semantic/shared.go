package semantic

import (
	"context"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/embed"
	"github.com/kgatilin/muninn/internal/index"
	"github.com/kgatilin/muninn/internal/usage"
)

const embedBatch = 256

// embedPhrases embeds the phrases, keyed by content hash, into the bank's
// vector store.
func embedPhrases(ctx context.Context, b *bank.Bank, embedder embed.Embedder, vectors *index.Vectors, missing map[string]string) error {
	hashes := make([]string, 0, len(missing))
	for h := range missing {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	for start := 0; start < len(hashes); start += embedBatch {
		part := hashes[start:min(start+embedBatch, len(hashes))]
		texts := make([]string, len(part))
		for i, h := range part {
			texts[i] = missing[h]
		}
		vecs, tokens, err := embedder.Embed(ctx, texts)
		b.RecordUsage(usage.Embed, b.Embedder.String(), tokens)
		if err != nil {
			return err
		}
		if err := vectors.Append(part, vecs); err != nil {
			return err
		}
	}
	return nil
}

// dropWeakest is the revision that needs no thought. Over the entity budget,
// the patch loses as many of the entities it mints as the budget is over by,
// the weakest mentions first, and keeps its mentions of entities that exist.
// Otherwise, and when it mints nothing, it loses its lowest-scored mention and
// the entity added for it. False when no mention would be left.
func dropWeakest(rejected Patch, violations []Result) (Patch, bool) {
	minted := map[string]bool{}
	for _, op := range rejected.Ops {
		if op.Kind == OpAddEntity {
			minted[op.Entity] = true
		}
	}
	over := 0
	for _, v := range violations {
		if v.Check == "entity_budget" {
			over = max(1, int(math.Ceil(v.Delta-v.MaxRegression)))
		}
	}
	// The mentions by score, weakest first; the later of two equals goes first.
	var order []int
	for i, op := range rejected.Ops {
		if op.Kind == OpAddMention && (over == 0 || minted[op.Entity]) {
			order = append(order, i)
		}
	}
	if len(order) == 0 {
		if over == 0 {
			return Patch{}, false
		}
		return dropWeakest(rejected, nil)
	}
	sort.SliceStable(order, func(a, b int) bool {
		sa, sb := rejected.Ops[order[a]].Score, rejected.Ops[order[b]].Score
		if sa != sb {
			return sa < sb
		}
		return order[a] > order[b]
	})
	gone := map[string]bool{}
	for _, i := range order[:min(max(over, 1), len(order))] {
		gone[rejected.Ops[i].Entity] = true
	}
	var p Patch
	mentions := 0
	for _, op := range rejected.Ops {
		if (op.Kind == OpAddEntity || op.Kind == OpAddMention) && gone[op.Entity] {
			continue
		}
		if op.Kind == OpAddMention {
			mentions++
		}
		p.Ops = append(p.Ops, op)
	}
	return p, mentions > 0
}

// numeric is a number, or a token of seven runes or more that is all hex with
// a digit in it: a commit, a uuid's part.
func numeric(t string) bool {
	digits, hex := 0, true
	for _, r := range t {
		if unicode.IsDigit(r) {
			digits++
		} else if !strings.ContainsRune("abcdef", r) {
			hex = false
		}
	}
	n := utf8.RuneCountInString(t)
	return digits == n || (hex && digits > 0 && n >= 7)
}

var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(stopwordsEN + " " + stopwordsRU + " " + stopwordsCode) {
		m[w] = true
	}
	return m
}()

const stopwordsEN = `a about above after again against all also am an and any are aren as at be because been
before being below between both but by can cannot could couldn did didn do does doesn doing don down during each
else even ever every few for from further get gets got had hadn has hasn have haven having he her here hers
herself him himself his how however i if in into is isn it its itself just let like ll may me might more most
much must my myself need no nor not now of off on once one only onto or other ought our ours ourselves out over
own per same shall shan she should shouldn since so some such than that the their theirs them themselves then
there these they this those though through thus to too under until up upon us use used uses using very via was
wasn we were weren what when where whether which while who whom whose why will with within without won would
wouldn yes yet you your yours yourself yourselves new old way ways thing things make makes made want wants see
also still well back two first last many already around another something nothing here there`

// stopwordsCode are the keywords and the everyday identifiers of programming
// languages, which a bank with code or with talk about code is full of and
// which name nothing.
const stopwordsCode = `func err nil null none true false return var const struct fmt len str int bool byte bytes
string strings json yaml cmd ctx key keys line lines path paths file files dir dirs arg args param params tmp
foo bar def elif self void println printf sprintf errorf todo http https www com`

const stopwordsRU = `и в во не что он на я с со как а то все она так его но да ты к у же вы за бы по только ее её
мне было вот от меня еще ещё нет о из ему теперь когда даже ну вдруг ли если уже или ни быть был него до вас
нибудь опять уж вам ведь там потом себя ничего ей может они тут где есть надо ней для мы тебя их чем была сам
чтоб без будто чего раз тоже себе под будет ж тогда кто этот того потому этого какой совсем ним здесь этом один
почти мой тем чтобы нее неё сейчас были куда зачем всех никогда можно при наконец два об другой хоть после над
больше тот через эти нас про всего них какая много разве три эту моя впрочем хорошо свою этой перед иногда
лучше чуть том нельзя такой им более всегда конечно всю между это эта эти этих также которые который которая
которое которых нужно просто очень когда либо тогда пока более менее будут быть есть`
