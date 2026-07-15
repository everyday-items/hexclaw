package engine

import (
	"fmt"
	"math/big"
	"strings"
	"unicode"
)

// solveLinearEquation 对严格白名单内的单变量一次方程做本机精确求解。
//
// 安全边界：只接受数字、x/X、四则运算、括号和恰好一个等号；左右两边都必须能归约为
// ax+b。hasVar 单独记录“语法上曾含变量”，因此 (x-x)×x、1/(x-x) 不会因代数化简为
// 常数而绕过非线性/变量作除数检查。任何拿不准的输入都返回 ok=false，交回正常模型链。
func solveLinearEquation(problem string) (worked, answer string, ok bool) {
	leftText, rightText, ok := splitLinearEquation(problem)
	if !ok {
		return "", "", false
	}
	left, ok := parseLinearExpression(leftText)
	if !ok {
		return "", "", false
	}
	right, ok := parseLinearExpression(rightText)
	if !ok || (!left.hasVar && !right.hasVar) {
		return "", "", false
	}

	// (left.a-right.a)x = right.b-left.b。系数为 0 时分别是无解或无穷多解；两者都
	// 不应伪装成一个确定答案。
	coefficient := new(big.Rat).Sub(left.a, right.a)
	constant := new(big.Rat).Sub(right.b, left.b)
	if coefficient.Sign() == 0 {
		return "", "", false
	}
	// 展示时让 x 的系数为正，更符合小学阶段的阅读习惯。
	if coefficient.Sign() < 0 {
		coefficient.Neg(coefficient)
		constant.Neg(constant)
	}
	solution := new(big.Rat).Quo(constant, coefficient)
	answer = formatArithmeticRat(solution)
	if answer == "" {
		return "", "", false
	}

	leftValue := evalLinearForm(left, solution)
	rightValue := evalLinearForm(right, solution)
	if leftValue.Cmp(rightValue) != 0 { // 理论上不会发生；守住强证据出口。
		return "", "", false
	}

	var b strings.Builder
	b.WriteString("先把方程两边整理：\n")
	fmt.Fprintf(&b, "%s = %s\n\n", formatLinearForm(left), formatLinearForm(right))
	b.WriteString("根据等式的性质，把含 x 的项放在一边，把不含 x 的数放在另一边：\n")
	fmt.Fprintf(&b, "%sx = %s\n", formatLinearCoefficient(coefficient), formatArithmeticRat(constant))
	if coefficient.Cmp(big.NewRat(1, 1)) != 0 {
		fmt.Fprintf(&b, "x = %s ÷ %s\n", formatArithmeticRat(constant), formatArithmeticRat(coefficient))
	}
	fmt.Fprintf(&b, "x = %s\n\n", answer)
	fmt.Fprintf(&b, "验算：把 x = %s 代回原方程，\n", answer)
	fmt.Fprintf(&b, "左边 = %s，右边 = %s，左右相等。\n\n", formatArithmeticRat(leftValue), formatArithmeticRat(rightValue))
	fmt.Fprintf(&b, "答案：x = %s", answer)
	return b.String(), answer, true
}

func splitLinearEquation(problem string) (left, right string, ok bool) {
	s := strings.TrimSpace(problem)
	if s == "" || len(s) > 256 {
		return "", "", false
	}
	s = strings.NewReplacer(
		"×", "*", "÷", "/", "＋", "+", "－", "-", "−", "-",
		"（", "(", "）", ")", "＝", "=",
	).Replace(s)
	if strings.Count(s, "=") != 1 {
		return "", "", false
	}
	parts := strings.SplitN(s, "=", 2)
	left, right = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if left == "" || right == "" {
		return "", "", false
	}
	return left, right, true
}

type linearForm struct {
	a, b   *big.Rat // ax+b
	hasVar bool     // 语法上曾含 x；不能只看化简后的 a 是否为 0。
}

func linearConstant(v *big.Rat) linearForm {
	return linearForm{a: new(big.Rat), b: new(big.Rat).Set(v)}
}

func linearVariable() linearForm {
	return linearForm{a: big.NewRat(1, 1), b: new(big.Rat), hasVar: true}
}

func addLinear(left, right linearForm, subtract bool) linearForm {
	a, b := new(big.Rat), new(big.Rat)
	if subtract {
		a.Sub(left.a, right.a)
		b.Sub(left.b, right.b)
	} else {
		a.Add(left.a, right.a)
		b.Add(left.b, right.b)
	}
	return linearForm{a: a, b: b, hasVar: left.hasVar || right.hasVar}
}

func multiplyLinear(left, right linearForm) (linearForm, bool) {
	if left.hasVar && right.hasVar {
		return linearForm{}, false
	}
	result := linearForm{a: new(big.Rat), b: new(big.Rat), hasVar: left.hasVar || right.hasVar}
	switch {
	case left.hasVar:
		result.a.Mul(left.a, right.b)
		result.b.Mul(left.b, right.b)
	case right.hasVar:
		result.a.Mul(right.a, left.b)
		result.b.Mul(right.b, left.b)
	default:
		result.b.Mul(left.b, right.b)
	}
	return result, true
}

func divideLinear(numerator, denominator linearForm) (linearForm, bool) {
	if denominator.hasVar || denominator.b.Sign() == 0 {
		return linearForm{}, false
	}
	return linearForm{
		a:      new(big.Rat).Quo(numerator.a, denominator.b),
		b:      new(big.Rat).Quo(numerator.b, denominator.b),
		hasVar: numerator.hasVar,
	}, true
}

func negateLinear(value linearForm) linearForm {
	return linearForm{
		a:      new(big.Rat).Neg(value.a),
		b:      new(big.Rat).Neg(value.b),
		hasVar: value.hasVar,
	}
}

type linearTokenKind uint8

const (
	linearTokenInvalid linearTokenKind = iota
	linearTokenEOF
	linearTokenNumber
	linearTokenVariable
	linearTokenPlus
	linearTokenMinus
	linearTokenMultiply
	linearTokenDivide
	linearTokenLeftParen
	linearTokenRightParen
)

type linearToken struct {
	kind linearTokenKind
	lit  string
}

type linearLexer struct {
	input []rune
	pos   int
}

func (l *linearLexer) next() linearToken {
	for l.pos < len(l.input) && unicode.IsSpace(l.input[l.pos]) {
		l.pos++
	}
	if l.pos >= len(l.input) {
		return linearToken{kind: linearTokenEOF}
	}
	r := l.input[l.pos]
	l.pos++
	switch r {
	case 'x', 'X':
		return linearToken{kind: linearTokenVariable, lit: "x"}
	case '+':
		return linearToken{kind: linearTokenPlus, lit: "+"}
	case '-':
		return linearToken{kind: linearTokenMinus, lit: "-"}
	case '*':
		return linearToken{kind: linearTokenMultiply, lit: "*"}
	case '/':
		return linearToken{kind: linearTokenDivide, lit: "/"}
	case '(':
		return linearToken{kind: linearTokenLeftParen, lit: "("}
	case ')':
		return linearToken{kind: linearTokenRightParen, lit: ")"}
	}
	if r < '0' || r > '9' {
		return linearToken{kind: linearTokenInvalid, lit: string(r)}
	}
	start := l.pos - 1
	for l.pos < len(l.input) && l.input[l.pos] >= '0' && l.input[l.pos] <= '9' {
		l.pos++
	}
	if l.pos < len(l.input) && l.input[l.pos] == '.' {
		l.pos++
		decimalStart := l.pos
		for l.pos < len(l.input) && l.input[l.pos] >= '0' && l.input[l.pos] <= '9' {
			l.pos++
		}
		if l.pos == decimalStart { // 拒绝 1.，避免宽松猜测 OCR 残缺输入。
			return linearToken{kind: linearTokenInvalid}
		}
	}
	return linearToken{kind: linearTokenNumber, lit: string(l.input[start:l.pos])}
}

type linearParser struct {
	lexer linearLexer
	cur   linearToken
}

func parseLinearExpression(input string) (linearForm, bool) {
	p := &linearParser{lexer: linearLexer{input: []rune(input)}}
	p.advance()
	value, ok := p.parseExpression()
	if !ok || p.cur.kind != linearTokenEOF {
		return linearForm{}, false
	}
	return value, true
}

func (p *linearParser) advance() { p.cur = p.lexer.next() }

func (p *linearParser) parseExpression() (linearForm, bool) {
	left, ok := p.parseTerm()
	if !ok {
		return linearForm{}, false
	}
	for p.cur.kind == linearTokenPlus || p.cur.kind == linearTokenMinus {
		subtract := p.cur.kind == linearTokenMinus
		p.advance()
		right, ok := p.parseTerm()
		if !ok {
			return linearForm{}, false
		}
		left = addLinear(left, right, subtract)
	}
	return left, true
}

func (p *linearParser) parseTerm() (linearForm, bool) {
	left, ok := p.parseUnary()
	if !ok {
		return linearForm{}, false
	}
	for {
		op := p.cur.kind
		implicit := op == linearTokenVariable || op == linearTokenLeftParen
		if op != linearTokenMultiply && op != linearTokenDivide && !implicit {
			return left, true
		}
		if implicit {
			op = linearTokenMultiply
		} else {
			p.advance()
		}
		right, ok := p.parseUnary()
		if !ok {
			return linearForm{}, false
		}
		if op == linearTokenDivide {
			left, ok = divideLinear(left, right)
		} else {
			left, ok = multiplyLinear(left, right)
		}
		if !ok {
			return linearForm{}, false
		}
	}
}

func (p *linearParser) parseUnary() (linearForm, bool) {
	switch p.cur.kind {
	case linearTokenPlus:
		p.advance()
		return p.parseUnary()
	case linearTokenMinus:
		p.advance()
		value, ok := p.parseUnary()
		if !ok {
			return linearForm{}, false
		}
		return negateLinear(value), true
	default:
		return p.parsePrimary()
	}
}

func (p *linearParser) parsePrimary() (linearForm, bool) {
	switch p.cur.kind {
	case linearTokenNumber:
		value, ok := new(big.Rat).SetString(p.cur.lit)
		p.advance()
		if !ok {
			return linearForm{}, false
		}
		return linearConstant(value), true
	case linearTokenVariable:
		p.advance()
		return linearVariable(), true
	case linearTokenLeftParen:
		p.advance()
		value, ok := p.parseExpression()
		if !ok || p.cur.kind != linearTokenRightParen {
			return linearForm{}, false
		}
		p.advance()
		return value, true
	default:
		return linearForm{}, false
	}
}

func evalLinearForm(form linearForm, x *big.Rat) *big.Rat {
	return new(big.Rat).Add(new(big.Rat).Mul(form.a, x), form.b)
}

func formatLinearForm(form linearForm) string {
	if form.a.Sign() == 0 {
		return formatArithmeticRat(form.b)
	}
	var b strings.Builder
	switch form.a.Cmp(big.NewRat(1, 1)) {
	case 0:
		b.WriteString("x")
	default:
		if form.a.Cmp(big.NewRat(-1, 1)) == 0 {
			b.WriteString("-x")
		} else {
			b.WriteString(formatArithmeticRat(form.a))
			b.WriteString("x")
		}
	}
	if form.b.Sign() > 0 {
		b.WriteString(" + ")
		b.WriteString(formatArithmeticRat(form.b))
	} else if form.b.Sign() < 0 {
		b.WriteString(" - ")
		b.WriteString(formatArithmeticRat(new(big.Rat).Abs(form.b)))
	}
	return b.String()
}

func formatLinearCoefficient(value *big.Rat) string {
	if value.Cmp(big.NewRat(1, 1)) == 0 {
		return ""
	}
	return formatArithmeticRat(value)
}

// linearEquationAllowedByConstraint 沿用纯算式的运算范围检查，并额外要求白名单明确包含
// 方程知识点。空 constraint 与纯算式一致，表示调用方没有可用白名单，不在这里臆造超纲结论。
func linearEquationAllowedByConstraint(problem, constraint string) bool {
	if !trivialArithmeticAllowedByConstraint(problem, constraint) {
		return false
	}
	c := strings.TrimSpace(constraint)
	if c == "" {
		return true
	}
	for _, allowed := range []string{"简易方程", "解方程", "一元一次方程", "等式的性质"} {
		if strings.Contains(c, allowed) {
			return true
		}
	}
	return false
}
