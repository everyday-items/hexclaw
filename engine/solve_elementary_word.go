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
	inverseFractionProblemRe = regexp.MustCompile(`^(?:[0-9]+[.．、])?一个数的([0-9]+)/([0-9]+)是` + elementaryNumberPattern + `[，,。.]?(?:求(?:这个数|原数)(?:是多少)?|(?:这个数|原数)(?:是)?多少)[?？。.]?$`)
	successiveFractionRe     = regexp.MustCompile(`^(?:[0-9]+[.．、])?` + elementaryNumberPattern + `的([0-9]+)/([0-9]+)的([0-9]+)/([0-9]+)(?:是)?多少[?？。.]?$`)
	rectangleYieldRe         = regexp.MustCompile(`^(?:[0-9]+[.．、])?(?:一个)?周长(?:是|为)?` + elementaryNumberPattern + `米的长方形(?:鱼塘)?[，,。.]?长是宽的` + elementaryNumberPattern + `倍[，,。.]?(?:如果)?每平方米(?:鱼塘)?(?:可)?产鱼` + elementaryNumberPattern + `千克[，,。.]?(?:一共|总共)(?:可|能)?产鱼多少千克[?？。.]?$`)

	finalQuantityMarkerRe = regexp.MustCompile(`(?i)(?:答案?|答)\s*(?:是|为)?\s*[:：]?\s*([+\-]?[0-9]+(?:\.[0-9]+)?(?:/[0-9]+)?)\s*(平方米|千克|公斤|m²|m2|kg|克|米|g|m)?`)
	bareQuantityRe        = regexp.MustCompile(`(?i)^\s*([+\-]?[0-9]+(?:\.[0-9]+)?(?:/[0-9]+)?)\s*(平方米|千克|公斤|m²|m2|kg|克|米|g|m)?\s*$`)
	equationQuantityRe    = regexp.MustCompile(`(?i)[=＝]\s*([+\-]?[0-9]+(?:\.[0-9]+)?(?:/[0-9]+)?)\s*(平方米|千克|公斤|m²|m2|kg|克|米|g|m)?`)
	studentEquationRe     = regexp.MustCompile(`([+\-]?[0-9][0-9.\s()（）\[\]+\-×÷*/]*?)\s*[=＝]\s*([+\-]?[0-9]+(?:\.[0-9]+)?(?:/[0-9]+)?)`)
)

type elementaryWordSolution struct {
	worked         string
	value          string
	unit           string
	knowledgePoint string
}

type answerQuantity struct {
	value string
	unit  string
}

// solveElementaryWordProblem 只覆盖可由题面数字唯一确定、且结构明确的小学数量关系。
// 正则锚定完整题型和所问目标；任何额外条件、目标变化、缺条件或歧义都返回 false，绝不猜答案。
func solveElementaryWordProblem(problem string) (worked, answer string, ok bool) {
	solution, ok := solveElementaryWordProblemDetailed(problem)
	if !ok {
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

	return elementaryWordSolution{}, false
}

func compactElementaryProblem(problem string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(problem))
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
	return false
}

func parseAnswerQuantity(answer string) (answerQuantity, bool) {
	if strings.TrimSpace(answer) == "" || len(answer) > 1024 {
		return answerQuantity{}, false
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
		return answerQuantity{}, false
	}
	_, value, ok := solveTrivialArithmetic(match[1])
	if !ok {
		return answerQuantity{}, false
	}
	return answerQuantity{value: value, unit: normalizeAnswerUnit(match[2])}, true
}

func normalizeAnswerUnit(unit string) string {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "千克", "公斤", "kg":
		return "千克"
	case "克", "g":
		return "克"
	case "平方米", "m²", "m2":
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
func validateStudentArithmeticWork(answer string) (valid, conclusive bool) {
	equationCount := strings.Count(answer, "=") + strings.Count(answer, "＝")
	if equationCount == 0 {
		return true, true
	}
	matches := studentEquationRe.FindAllStringSubmatch(answer, -1)
	if len(matches) != equationCount {
		return false, false
	}
	for _, match := range matches {
		lhs := strings.NewReplacer("[", "(", "]", ")", "（", "(", "）", ")").Replace(strings.TrimSpace(match[1]))
		_, left, leftOK := solveTrivialArithmetic(lhs)
		_, right, rightOK := solveTrivialArithmetic(strings.TrimSpace(match[2]))
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
