package engine

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"unicode"
)

const elementaryNumberPattern = `([0-9]+(?:\.[0-9]+)?)`

var (
	elementaryParenthesizedFractionRe = regexp.MustCompile(`\(([0-9]+)\)/\(([0-9]+)\)`)
	inverseFractionProblemRe          = regexp.MustCompile(`^(?:[0-9]+[.．、])?一个数的([0-9]+)/([0-9]+)是` + elementaryNumberPattern + `[，,。.]?(?:求(?:这个数|原数)(?:是多少)?|(?:这个数|原数)(?:是)?多少)[?？。.]?$`)
	successiveFractionRe              = regexp.MustCompile(`^(?:[0-9]+[.．、])?` + elementaryNumberPattern + `的([0-9]+)/([0-9]+)的([0-9]+)/([0-9]+)(?:是)?多少[?？。.]?$`)
	rectangleYieldRe                  = regexp.MustCompile(`^(?:[0-9]+[.．、])?(?:一个)?周长(?:是|为)?` + elementaryNumberPattern + `米的长方形(?:鱼塘)?[，,。.]?长是宽的` + elementaryNumberPattern + `倍[，,。.]?(?:如果)?每平方米(?:鱼塘)?(?:可)?产鱼` + elementaryNumberPattern + `千克[，,。.]?(?:一共|总共)(?:可|能)?产鱼多少千克[?？。.]?$`)
	openCubeFishTankRe                = regexp.MustCompile(`^(?:小明的爸爸)?用玻璃做了一个棱长(?:是|为)?` + elementaryNumberPattern + `(?:dm|分米)的正方体鱼缸[。.]制作(?:这个|该)鱼缸时[，,]?至少需要玻璃多少平方米[?？](?:小明)?在鱼缸里注入` + elementaryNumberPattern + `(?:L|l|升)的水[，,]?水面高度(?:是|为)?多少分米[?？。.]?$`)
	ticketGCDLCMRe                    = regexp.MustCompile(`^(?:小明)?有(?:一)?张([0-9]+)至([0-9]+)排的电影票[，,]这张票的排数和座位号的最大公约数是([0-9]+)[，,]最小公倍数是([0-9]+)[，,。.](?:小明)?这张电影票是[（(][）)]排[（(][）)]号[。.]?$`)
	sixNumberBalanceRe                = regexp.MustCompile(`^(?:[0-9]+[.．、])?在下列六个数[:：]([0-9]+)[、,，]([0-9]+)[、,，]([0-9]+)[、,，]([0-9]+)[、,，]([0-9]+)[、,，]([0-9]+)中划去(?:一个)?数[（(]?[）)]?后[，,]?能使其中3个数的和(?:是|为)?另外2个数(?:的)?和的2倍[。.]?$`)

	finalQuantityMarkerRe = regexp.MustCompile(`(?i)(?:答案?|答)\s*(?:是|为)?\s*[:：]?\s*([+\-]?[0-9]+(?:\.[0-9]+)?(?:/[0-9]+)?)\s*(平方米|千克|公斤|m²|m2|kg|克|米|g|m)?`)
	removedNumberMarkerRe = regexp.MustCompile(`划去(?:数)?\s*[:：]?\s*([+\-]?[0-9]+)`)
	bareQuantityRe        = regexp.MustCompile(`(?i)^\s*([+\-]?[0-9]+(?:\.[0-9]+)?(?:/[0-9]+)?)\s*(平方米|千克|公斤|m²|m2|kg|克|米|g|m)?\s*$`)
	equationQuantityRe    = regexp.MustCompile(`(?i)[=＝]\s*([+\-]?[0-9]+(?:\.[0-9]+)?(?:/[0-9]+)?)(?:\s*(?:[（(]\s*)?(平方米|千克|公斤|m²|m\^?2|kg|克|米|g|m)(?:\s*[）)])?)?`)
	equationUnitSuffixRe  = regexp.MustCompile(`(?i)\s*(?:[（(]\s*)?(?:平方米|千克|公斤|m²|m\^?2|kg|克|米|g|m)(?:\s*[）)])?\s*$`)
)

type elementaryWordSolution struct {
	worked         string
	value          string
	unit           string
	knowledgePoint string
	problemIssue   string
}

type answerQuantity struct {
	value string
	unit  string
}

// solveElementaryWordProblem 只覆盖可由题面数字唯一确定、且结构明确的小学数量关系。
// 正则锚定完整题型和所问目标；任何额外条件、目标变化、缺条件或歧义都返回 false，绝不猜答案。
func solveElementaryWordProblem(problem string) (worked, answer string, ok bool) {
	solution, ok := solveElementaryWordProblemDetailed(problem)
	if !ok || solution.problemIssue != "" {
		return "", "", false
	}
	return solution.worked, solution.value, true
}

func solveElementaryWordProblemDetailed(problem string) (elementaryWordSolution, bool) {
	problem = compactElementaryProblem(problem)
	if m := inverseFractionProblemRe.FindStringSubmatch(problem); len(m) == 4 {
		numerator, numeratorOK := positiveRat(m[1])
		denominator, denominatorOK := positiveRat(m[2])
		known, knownOK := nonNegativeRat(m[3])
		if !numeratorOK || !denominatorOK || !knownOK {
			return elementaryWordSolution{}, false
		}
		value := new(big.Rat).Mul(known, denominator)
		value.Quo(value, numerator)
		answer := formatArithmeticRat(value)
		// 五年级下只用已经学过的整数乘除表达“先求一份，再求若干份”，不展示六上分数除法。
		worked := fmt.Sprintf("把这个数平均分成 %s 份，%s 份是 %s。\n先求 1 份，再求 %s 份：\n%s÷%s×%s = %s\n\n答案：%s",
			m[2], m[1], m[3], m[2], m[3], m[1], m[2], answer, answer)
		return elementaryWordSolution{worked: worked, value: answer, knowledgePoint: "分数的意义和性质"}, true
	}

	if m := successiveFractionRe.FindStringSubmatch(problem); len(m) == 6 {
		base, bOK := nonNegativeRat(m[1])
		n1, n1OK := nonNegativeRat(m[2])
		d1, d1OK := positiveRat(m[3])
		n2, n2OK := nonNegativeRat(m[4])
		d2, d2OK := positiveRat(m[5])
		if !bOK || !n1OK || !d1OK || !n2OK || !d2OK {
			return elementaryWordSolution{}, false
		}
		value := new(big.Rat).Mul(base, n1)
		value.Quo(value, d1)
		value.Mul(value, n2)
		value.Quo(value, d2)
		answer := formatArithmeticRat(value)
		worked := fmt.Sprintf("按“先除以分母，再乘分子”依次计算：\n%s÷%s×%s÷%s×%s = %s\n\n答案：%s",
			m[1], m[3], m[2], m[5], m[4], answer, answer)
		return elementaryWordSolution{worked: worked, value: answer, knowledgePoint: "分数的意义和性质"}, true
	}

	if m := rectangleYieldRe.FindStringSubmatch(problem); len(m) == 4 {
		perimeter, pOK := positiveRat(m[1])
		ratio, rOK := positiveRat(m[2])
		yield, yOK := nonNegativeRat(m[3])
		if !pOK || !rOK || !yOK {
			return elementaryWordSolution{}, false
		}
		two := big.NewRat(2, 1)
		ratioPlusOne := new(big.Rat).Add(ratio, big.NewRat(1, 1))
		width := new(big.Rat).Quo(perimeter, new(big.Rat).Mul(two, ratioPlusOne))
		length := new(big.Rat).Mul(width, ratio)
		area := new(big.Rat).Mul(width, length)
		total := new(big.Rat).Mul(area, yield)
		answer := formatArithmeticRat(total)
		worked := fmt.Sprintf("长方形周长 = 2×(长+宽)，长是宽的 %s 倍。\n宽：%s÷[2×(%s+1)] = %s 米\n长：%s×%s = %s 米\n面积：%s×%s = %s 平方米\n产量：%s×%s = %s 千克\n\n答案：%s 千克",
			m[2], m[1], m[2], formatArithmeticRat(width), formatArithmeticRat(width), m[2], formatArithmeticRat(length),
			formatArithmeticRat(width), formatArithmeticRat(length), formatArithmeticRat(area), formatArithmeticRat(area), m[3], answer, answer)
		return elementaryWordSolution{worked: worked, value: answer, unit: "千克", knowledgePoint: "长方形的周长和面积"}, true
	}

	if m := openCubeFishTankRe.FindStringSubmatch(problem); len(m) == 3 {
		edge, edgeOK := positiveRat(m[1])
		water, waterOK := nonNegativeRat(m[2])
		if !edgeOK || !waterOK {
			return elementaryWordSolution{}, false
		}
		baseArea := new(big.Rat).Mul(edge, edge)
		glassDM2 := new(big.Rat).Mul(baseArea, big.NewRat(5, 1))
		glassM2 := new(big.Rat).Quo(glassDM2, big.NewRat(100, 1))
		waterHeight := new(big.Rat).Quo(water, baseArea)
		glassAnswer := formatArithmeticRat(glassM2)
		heightAnswer := formatArithmeticRat(waterHeight)
		answer := glassAnswer + "平方米；" + heightAnswer + "分米"
		worked := fmt.Sprintf("鱼缸无盖，只需计算底面和 4 个侧面，共 5 个面。\n玻璃面积：%s×%s×5 = %s 平方分米\n换算成平方米：%s÷100 = %s 平方米\n1 L = 1 立方分米，水的体积是 %s 立方分米。\n底面积：%s×%s = %s 平方分米\n水面高度：%s÷(%s×%s) = %s 分米\n\n答案：%s平方米；%s分米（依次为至少需要的玻璃面积、水面高度）。",
			m[1], m[1], formatArithmeticRat(glassDM2), formatArithmeticRat(glassDM2), glassAnswer,
			m[2], m[1], m[1], formatArithmeticRat(baseArea), m[2], m[1], m[1], heightAnswer, glassAnswer, heightAnswer)
		return elementaryWordSolution{worked: worked, value: answer, knowledgePoint: "长方体和正方体的表面积、体积和容积"}, true
	}

	if m := ticketGCDLCMRe.FindStringSubmatch(problem); len(m) == 5 {
		gcd, gcdOK := new(big.Int).SetString(m[3], 10)
		lcm, lcmOK := new(big.Int).SetString(m[4], 10)
		if !gcdOK || !lcmOK || gcd.Sign() <= 0 || lcm.Sign() <= 0 || new(big.Int).Mod(lcm, gcd).Sign() != 0 {
			issue := fmt.Sprintf("题目信息矛盾：最大公约数 %s 必须能整除最小公倍数 %s，但题面中这两个数不符合这个必要条件。请核对原题数值后再解题。", m[3], m[4])
			return elementaryWordSolution{problemIssue: issue, knowledgePoint: "最大公因数和最小公倍数"}, true
		}
		// 数值自洽不代表排数和座位号唯一，仍交给完整解题链，不在快路中猜答案。
		return elementaryWordSolution{}, false
	}

	if m := sixNumberBalanceRe.FindStringSubmatch(problem); len(m) == 7 {
		numbers := make([]int, 6)
		for i := range numbers {
			value, ok := new(big.Int).SetString(m[i+1], 10)
			if !ok || !value.IsInt64() {
				return elementaryWordSolution{}, false
			}
			numbers[i] = int(value.Int64())
		}
		type balanceMatch struct {
			removed int
			pair    [2]int
			triple  [3]int
		}
		matchesByRemoved := make(map[int]balanceMatch)
		for removedIndex, removed := range numbers {
			remaining := make([]int, 0, 5)
			for i, value := range numbers {
				if i != removedIndex {
					remaining = append(remaining, value)
				}
			}
			for left := 0; left < len(remaining); left++ {
				for right := left + 1; right < len(remaining); right++ {
					pairSum := remaining[left] + remaining[right]
					triple := [3]int{}
					tripleIndex := 0
					tripleSum := 0
					for i, value := range remaining {
						if i == left || i == right {
							continue
						}
						triple[tripleIndex] = value
						tripleIndex++
						tripleSum += value
					}
					if tripleSum == 2*pairSum {
						matchesByRemoved[removed] = balanceMatch{
							removed: removed,
							pair:    [2]int{remaining[left], remaining[right]},
							triple:  triple,
						}
					}
				}
			}
		}
		if len(matchesByRemoved) != 1 {
			return elementaryWordSolution{}, false
		}
		var matched balanceMatch
		for _, candidate := range matchesByRemoved {
			matched = candidate
		}
		answer := fmt.Sprintf("%d", matched.removed)
		worked := fmt.Sprintf("先分别尝试划去一个数，再把剩下的 5 个数分成 3 个数和 2 个数两组。\n划去 %d 后：\n%d+%d+%d = %d\n%d+%d = %d\n%d = %d×2\n\n答案：划去 %d。",
			matched.removed,
			matched.triple[0], matched.triple[1], matched.triple[2], matched.triple[0]+matched.triple[1]+matched.triple[2],
			matched.pair[0], matched.pair[1], matched.pair[0]+matched.pair[1],
			matched.triple[0]+matched.triple[1]+matched.triple[2], matched.pair[0]+matched.pair[1], matched.removed)
		return elementaryWordSolution{worked: worked, value: answer, knowledgePoint: "数的组合与倍数关系"}, true
	}

	return elementaryWordSolution{}, false
}

func compactElementaryProblem(problem string) string {
	compact := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(problem))
	return elementaryParenthesizedFractionRe.ReplaceAllString(compact, "$1/$2")
}

func elementaryWordAllowedByConstraint(problem, constraint string) bool {
	p := compactElementaryProblem(problem)
	c := strings.TrimSpace(constraint)
	if c == "" {
		return true
	}
	if inverseFractionProblemRe.MatchString(p) || successiveFractionRe.MatchString(p) {
		return strings.Contains(c, "分数")
	}
	if rectangleYieldRe.MatchString(p) {
		return strings.Contains(c, "长方形") || strings.Contains(c, "面积") || strings.Contains(c, "周长")
	}
	if openCubeFishTankRe.MatchString(p) {
		return strings.Contains(c, "正方体") || strings.Contains(c, "表面积") || strings.Contains(c, "体积") || strings.Contains(c, "容积")
	}
	if ticketGCDLCMRe.MatchString(p) {
		hasGCD := strings.Contains(c, "最大公约数") || strings.Contains(c, "最大公因数")
		return hasGCD && strings.Contains(c, "最小公倍数")
	}
	if sixNumberBalanceRe.MatchString(p) {
		return strings.Contains(c, "整数") || strings.Contains(c, "倍数") ||
			strings.Contains(c, "加法") || strings.Contains(c, "乘法") ||
			strings.Contains(c, "数的组合")
	}
	return false
}

func parseAnswerQuantity(answer string) (answerQuantity, bool) {
	if strings.TrimSpace(answer) == "" || len(answer) > 1024 {
		return answerQuantity{}, false
	}
	if matches := removedNumberMarkerRe.FindAllStringSubmatch(answer, -1); len(matches) > 0 {
		match := matches[len(matches)-1]
		_, value, ok := solveTrivialArithmetic(match[1])
		if !ok {
			return answerQuantity{}, false
		}
		return answerQuantity{value: value}, true
	}
	var match []string
	if matches := finalQuantityMarkerRe.FindAllStringSubmatch(answer, -1); len(matches) > 0 {
		match = matches[len(matches)-1]
	} else if matches := equationQuantityRe.FindAllStringSubmatch(answer, -1); len(matches) > 0 {
		match = matches[len(matches)-1]
	} else {
		match = bareQuantityRe.FindStringSubmatch(answer)
	}
	if len(match) != 3 {
		if value, ok := arithmeticAnswerValue(answer); ok {
			return answerQuantity{value: value}, true
		}
		return answerQuantity{}, false
	}
	unit := normalizeAnswerUnit(match[2])
	if unit == "" {
		if value, ok := arithmeticAnswerValue(answer); ok {
			return answerQuantity{value: value}, true
		}
	}
	_, value, ok := solveTrivialArithmetic(match[1])
	if !ok {
		return answerQuantity{}, false
	}
	return answerQuantity{value: value, unit: unit}, true
}

func normalizeAnswerUnit(unit string) string {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "千克", "公斤", "kg":
		return "千克"
	case "克", "g":
		return "克"
	case "平方米", "m²", "m2", "m^2":
		return "平方米"
	case "米", "m":
		return "米"
	default:
		return ""
	}
}

func quantitiesEqual(a, b answerQuantity) bool {
	return a.value == b.value && a.unit == b.unit
}

// validateStudentArithmeticWork 对学生已经写出的纯数值等式逐条复算。若有等号却无法完整、
// 保守地解析，则返回 conclusive=false 交给 grader；能确认某一步算错时才本地判错。
func validateStudentArithmeticWork(problem, answer string) (valid, conclusive bool) {
	answer = strings.ReplaceAll(answer, "＝", "=")
	previous := ""
	if expr, _, ok := normalizeTrivialArithmetic(problem); ok {
		previous = expr
	}
	pairs := make([][2]string, 0, strings.Count(answer, "="))
	for _, line := range strings.FieldsFunc(answer, func(r rune) bool {
		return r == '\n' || r == ';' || r == '；' || r == '，'
	}) {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "=") {
			continue
		}
		parts := strings.Split(line, "=")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		if parts[0] == "" {
			if previous == "" {
				return false, false
			}
			parts[0] = previous
		}
		for i := 0; i+1 < len(parts); i++ {
			if parts[i] == "" || parts[i+1] == "" {
				return false, false
			}
			pairs = append(pairs, [2]string{parts[i], parts[i+1]})
		}
		previous = parts[len(parts)-1]
	}
	if len(pairs) == 0 {
		return true, true
	}
	for _, pair := range pairs {
		lhs := strings.NewReplacer("[", "(", "]", ")", "（", "(", "）", ")").Replace(strings.TrimSpace(pair[0]))
		if firstDigit := strings.IndexFunc(lhs, unicode.IsDigit); firstDigit > 0 {
			prefix := lhs[:firstDigit]
			if strings.IndexFunc(prefix, func(r rune) bool {
				return unicode.IsLetter(r) || r == ':' || r == '：'
			}) >= 0 {
				lhs = lhs[firstDigit:]
			}
		}
		lhs = strings.Trim(lhs, "。.；;，,、 ")
		rhs := equationUnitSuffixRe.ReplaceAllString(strings.TrimSpace(pair[1]), "")
		rhs = strings.Trim(rhs, "。.；;，,、 ")
		_, left, leftOK := solveTrivialArithmetic(lhs)
		_, right, rightOK := solveTrivialArithmetic(rhs)
		if !leftOK || !rightOK {
			return false, false
		}
		if left != right {
			return false, true
		}
	}
	return true, true
}

func positiveRat(raw string) (*big.Rat, bool) {
	v, ok := new(big.Rat).SetString(raw)
	return v, ok && v.Sign() > 0
}

func nonNegativeRat(raw string) (*big.Rat, bool) {
	v, ok := new(big.Rat).SetString(raw)
	return v, ok && v.Sign() >= 0
}
