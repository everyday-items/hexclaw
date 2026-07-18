package engine

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math/big"
	"regexp"
	"strings"
	"unicode"
)

var writtenFinalAnswerRe = regexp.MustCompile(`(?:答案?|答)[^0-9+\-]*([+\-]?[0-9]+(?:\.[0-9]+)?(?:/[0-9]+)?)`)

// itemNumberPrefixRe 题号列表前缀（bug 2026-07-18：照片识别题干自带「1. 」「3、」「4)」等
// 题号，去空白后「1. 26*3」曾被误拼成小数「1.26*3」）。两类可安全剥离的形态：
//   - 数字 + 顿号/右括号（、)）——这些分隔符不可能是小数点；
//   - 数字 + 半角/全角句点 + 空白——小数写法不会在小数点后带空白。
var itemNumberPrefixRe = regexp.MustCompile(`^[0-9]{1,3}\s*(?:[、)）]|[.．]\s)\s*`)
var mixedNumberAnswerRe = regexp.MustCompile(`^([+\-]?)([0-9]+)(?:\s+|又)([0-9]+)\s*/\s*([0-9]+)$`)

// solveTrivialArithmetic 对“只含数字、四则运算、括号，等号右侧为空/问号”的一步算式做
// 本机精确求值。它刻意不接受变量、函数、单位或自然语言，避免把方程/应用题误判成纯计算。
func solveTrivialArithmetic(problem string) (worked, answer string, ok bool) {
	expr, display, ok := normalizeTrivialArithmetic(problem)
	if !ok {
		return "", "", false
	}
	node, err := parser.ParseExpr(expr)
	if err != nil {
		return "", "", false
	}
	remaining := 128
	value, ok := evalArithmeticNode(node, &remaining)
	if !ok {
		return "", "", false
	}
	answer = formatArithmeticRat(value)
	if answer == "" {
		return "", "", false
	}
	worked = fmt.Sprintf("按四则运算规则计算：\n%s = %s\n\n答案：%s", display, answer, answer)
	return worked, answer, true
}

func normalizeTrivialArithmetic(problem string) (expr, display string, ok bool) {
	s := strings.TrimSpace(problem)
	if s == "" || len(s) > 256 {
		return "", "", false
	}
	replacer := strings.NewReplacer(
		"×", "*", "÷", "/", "＋", "+", "－", "-", "−", "-",
		"（", "(", "）", ")", "＝", "=", "？", "?",
	)
	s = replacer.Replace(s)
	s = itemNumberPrefixRe.ReplaceAllString(s, "")
	if i := strings.IndexByte(s, '='); i >= 0 {
		// 只接受“表达式=”或“表达式=?”；已有等式/方程不是纯求值题。
		rhs := strings.TrimSpace(s[i+1:])
		if rhs != "" && rhs != "?" {
			return "", "", false
		}
		if strings.ContainsRune(s[i+1:], '=') {
			return "", "", false
		}
		s = strings.TrimSpace(s[:i])
	} else {
		s = strings.TrimSuffix(strings.TrimSpace(s), "?")
	}
	if s == "" {
		return "", "", false
	}
	hasDigit := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case unicode.IsSpace(r), strings.ContainsRune(".+-*/()", r):
		default:
			return "", "", false
		}
	}
	if !hasDigit {
		return "", "", false
	}
	expr = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
	display = strings.NewReplacer("*", "×", "/", "÷").Replace(expr)
	return expr, display, true
}

// arithmeticAnswerValue 只从“纯数值/算式答案”中取精确值。允许学生写完整等式或在最后一行写
// “答案：…”，但不剥单位、不从自然语言里猜数字；无法保守判定时仍交给 grader。
func arithmeticAnswerValue(answer string) (string, bool) {
	s := strings.TrimSpace(answer)
	if s == "" || len(s) > 512 {
		return "", false
	}
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(lines[i])
		candidate = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(candidate, "答案："), "答："))
		candidate = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(candidate, "答案:"), "答:"))
		candidate = strings.ReplaceAll(candidate, "＝", "=")
		if eq := strings.LastIndexByte(candidate, '='); eq >= 0 {
			rhs := strings.TrimSpace(candidate[eq+1:])
			if rhs == "" || rhs == "?" {
				continue
			}
			candidate = rhs
		}
		if value, ok := mixedNumberAnswerValue(candidate); ok {
			return value, true
		}
		if _, value, ok := solveTrivialArithmetic(candidate); ok {
			return value, true
		}
	}
	// 有完整演算时最后一行不一定是纯算式，例如“24÷3×8=64 答这个数是64”。只在出现明确
	// “答/答案”标记时提取其后的首个纯数值；单位与其他文字不参与数值相等性比较。
	if matches := writtenFinalAnswerRe.FindAllStringSubmatch(s, -1); len(matches) > 0 {
		candidate := matches[len(matches)-1][1]
		if _, value, ok := solveTrivialArithmetic(candidate); ok {
			return value, true
		}
	}
	return "", false
}

// mixedNumberAnswerValue parses elementary-school mixed numbers such as "6 2/7" or "6又2/7".
// Passing the space-separated form through the ordinary expression normalizer would remove the
// space and silently reinterpret it as 62/7 instead of 6+2/7.
func mixedNumberAnswerValue(answer string) (string, bool) {
	matches := mixedNumberAnswerRe.FindStringSubmatch(strings.TrimSpace(answer))
	if len(matches) != 5 {
		return "", false
	}
	whole, wholeOK := new(big.Int).SetString(matches[2], 10)
	numerator, numeratorOK := new(big.Int).SetString(matches[3], 10)
	denominator, denominatorOK := new(big.Int).SetString(matches[4], 10)
	if !wholeOK || !numeratorOK || !denominatorOK || denominator.Sign() <= 0 ||
		numerator.Sign() < 0 || numerator.Cmp(denominator) >= 0 {
		return "", false
	}
	totalNumerator := new(big.Int).Add(new(big.Int).Mul(whole, denominator), numerator)
	if matches[1] == "-" {
		totalNumerator.Neg(totalNumerator)
	}
	return formatArithmeticRat(new(big.Rat).SetFrac(totalNumerator, denominator)), true
}

func evalArithmeticNode(node ast.Expr, remaining *int) (*big.Rat, bool) {
	if remaining == nil || *remaining <= 0 {
		return nil, false
	}
	(*remaining)--
	switch n := node.(type) {
	case *ast.ParenExpr:
		return evalArithmeticNode(n.X, remaining)
	case *ast.BasicLit:
		if n.Kind != token.INT && n.Kind != token.FLOAT {
			return nil, false
		}
		v, ok := new(big.Rat).SetString(n.Value)
		return v, ok
	case *ast.UnaryExpr:
		v, ok := evalArithmeticNode(n.X, remaining)
		if !ok {
			return nil, false
		}
		switch n.Op {
		case token.ADD:
			return new(big.Rat).Set(v), true
		case token.SUB:
			return new(big.Rat).Neg(v), true
		default:
			return nil, false
		}
	case *ast.BinaryExpr:
		left, ok := evalArithmeticNode(n.X, remaining)
		if !ok {
			return nil, false
		}
		right, ok := evalArithmeticNode(n.Y, remaining)
		if !ok {
			return nil, false
		}
		switch n.Op {
		case token.ADD:
			return new(big.Rat).Add(left, right), true
		case token.SUB:
			return new(big.Rat).Sub(left, right), true
		case token.MUL:
			return new(big.Rat).Mul(left, right), true
		case token.QUO:
			if right.Sign() == 0 {
				return nil, false
			}
			return new(big.Rat).Quo(left, right), true
		default:
			return nil, false
		}
	default:
		return nil, false
	}
}

func formatArithmeticRat(v *big.Rat) string {
	if v == nil {
		return ""
	}
	if v.IsInt() {
		return v.Num().String()
	}
	den := new(big.Int).Set(v.Denom())
	two, five := big.NewInt(2), big.NewInt(5)
	twos, fives := 0, 0
	for new(big.Int).Mod(den, two).Sign() == 0 {
		den.Quo(den, two)
		twos++
	}
	for new(big.Int).Mod(den, five).Sign() == 0 {
		den.Quo(den, five)
		fives++
	}
	if den.Cmp(big.NewInt(1)) != 0 {
		return v.RatString()
	}
	scale := twos
	if fives > scale {
		scale = fives
	}
	if scale > 64 {
		return v.RatString()
	}
	s := v.FloatString(scale)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "-0" {
		return "0"
	}
	return s
}

func trivialArithmeticAllowedByConstraint(problem, constraint string) bool {
	c := strings.TrimSpace(constraint)
	if c == "" {
		return true
	}
	p := strings.TrimSpace(problem)
	if strings.Contains(p, ".") && !strings.Contains(c, "小数") && !strings.Contains(c, "四则运算") {
		return false
	}
	if strings.ContainsAny(p, "÷/") &&
		!strings.Contains(c, "除法") && !strings.Contains(c, "分数") && !strings.Contains(c, "四则运算") {
		return false
	}
	if strings.ContainsAny(p, "×*") && !strings.Contains(c, "乘法") && !strings.Contains(c, "四则运算") {
		return false
	}
	return true
}
