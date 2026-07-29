# Base −2 Calculator

Write a **single-file Go program** (standard library only) that reads **one line**
from stdin — an arithmetic expression — evaluates it exactly, and prints the
result to stdout as a single line, followed by a newline.

## Number system

All numbers — literals in the input and the printed result — are written in
**negabinary** (base −2). A digit string `d_{k-1} … d_1 d_0` (each digit `0` or
`1`, leftmost most significant) denotes the integer

```
value = Σ  d_i · (−2)^i     (i = 0 … k−1)
```

Every integer, positive, negative, or zero, has exactly one negabinary
representation with no leading zeros (`0` for zero). Examples:

| decimal | negabinary |
|--------:|-----------:|
| 0       | `0`        |
| 1       | `1`        |
| 2       | `110`      |
| −1      | `11`       |
| −2      | `10`       |
| 3       | `111`      |
| 6       | `11010`    |

## Expressions

- **Literals**: negabinary digit strings, up to **200 digits**, no leading zeros
  (except `0` itself). Values therefore far exceed 64-bit range — you need
  arbitrary-precision arithmetic.
- **Operators**: binary `+`, `-`, `*`, `/`; unary `-`; parentheses `(` `)`.
- **Whitespace**: spaces and tabs may appear between any tokens.
- **Precedence** (tightest first):
  1. unary minus — applies to the immediately following primary (a literal, a
     parenthesized expression, or another unary minus; it may stack: `--1`),
  2. `*` and `/`, left-associative,
  3. binary `+` and `-`, left-associative.
- **Division** `/` is **floor division**: `a / b = ⌊a ÷ b⌋`, rounded toward
  **negative infinity** (so `-7 / 2 = -4`, `7 / -2 = -4`, `-7 / -2 = 3`).
  No division by zero ever occurs while evaluating any test expression.

## Output

The exact value of the expression, in negabinary: digits `0`/`1` only, **no
sign character** (negabinary needs none), **no leading zeros**, `0` for zero.

## Constraints

- Expression line length ≤ 20 000 characters.
- Intermediate and final values fit comfortably in arbitrary-precision integers.
- Per-test time limit: 10 seconds. Deterministic output required.
- The program must read from stdin and write to stdout, nothing else.

## Example

Input:

```
( 1 + 11 ) * 110
```

`1` = 1, `11` = −1, so `(1 + −1) * 2 = 0`.

Output:

```
0
```
