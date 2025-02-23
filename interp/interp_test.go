// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package interp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// runnerRunTimeout is the context timeout used by any tests calling Runner.Run.
// The timeout saves us from hangs or burning too much CPU if there are bugs.
// All the test cases are designed to be inexpensive and stop in a very short
// amount of time, so 5s should be plenty even for busy machines.
const runnerRunTimeout = 5 * time.Second

// Some program which should be in $PATH. Needs to run before runTests is
// initialized (so an init function wouldn't work), because runTest uses it.
var pathProg = func() string {
	if runtime.GOOS == "windows" {
		return "cmd"
	}
	return "sh"
}()

func parse(tb testing.TB, parser *syntax.Parser, src string) *syntax.File {
	if parser == nil {
		parser = syntax.NewParser()
	}
	file, err := parser.Parse(strings.NewReader(src), "")
	if err != nil {
		tb.Fatal(err)
	}
	return file
}

func BenchmarkRun(b *testing.B) {
	b.ReportAllocs()
	b.StopTimer()
	src := `
echo a b c d
echo ./$foo/etc $(echo foo bar)
foo="bar"
x=y :
fn() {
	local a=b
	for i in 1 2 3; do
		echo $i | cat
	done
}
[[ $foo == bar ]] && fn
echo a{b,c}d *.go
let i=(2 + 3)
`
	file := parse(b, nil, src)
	r, _ := New()
	ctx := context.Background()
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		r.Reset()
		if err := r.Run(ctx, file); err != nil {
			b.Fatal(err)
		}
	}
}

var hasBash50 bool

func TestMain(m *testing.M) {
	if os.Getenv("GOSH_PROG") != "" {
		switch os.Getenv("GOSH_CMD") {
		case "pid_and_hang":
			fmt.Println(os.Getpid())
			time.Sleep(time.Hour)
		case "foo_null_bar":
			fmt.Println("foo\x00bar")
			os.Exit(1)
		case "lookpath":
			_, err := exec.LookPath(pathProg)
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			fmt.Printf("%s found\n", pathProg)
			os.Exit(0)
		}
		r := strings.NewReader(os.Args[1])
		file, err := syntax.NewParser().Parse(r, "")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		runner, _ := New(
			StdIO(os.Stdin, os.Stdout, os.Stderr),
			OpenHandler(testOpenHandler),
			ExecHandler(testExecHandler),
		)
		ctx := context.Background()
		if err := runner.Run(ctx, file); err != nil {
			if status, ok := IsExitStatus(err); ok {
				os.Exit(int(status))
			}

			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		os.Exit(0)
	}
	prog, err := os.Executable()
	if err != nil {
		panic(err)
	}
	os.Setenv("GOSH_PROG", prog)

	os.Setenv("LANGUAGE", "en_US.UTF-8")
	os.Setenv("LC_ALL", "en_US.UTF-8")
	os.Unsetenv("CDPATH")
	hasBash50 = checkBash()

	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	os.Setenv("GO_TEST_DIR", wd)

	os.Setenv("INTERP_GLOBAL", "value")
	os.Setenv("MULTILINE_INTERP_GLOBAL", "\nwith\nnewlines\n\n")

	// Double check that env vars on Windows are case insensitive.
	if runtime.GOOS == "windows" {
		os.Setenv("mixedCase_INTERP_GLOBAL", "value")
	} else {
		os.Setenv("MIXEDCASE_INTERP_GLOBAL", "value")
	}

	os.Setenv("PATH_PROG", pathProg)

	// To print env vars. Only a builtin on Windows.
	if runtime.GOOS == "windows" {
		os.Setenv("ENV_PROG", "cmd /c set")
	} else {
		os.Setenv("ENV_PROG", "env")
	}

	for _, s := range []string{"a", "b", "c", "d", "foo", "bar"} {
		os.Unsetenv(s)
	}
	exit := m.Run()
	os.Exit(exit)
}

func checkBash() bool {
	out, err := exec.Command("bash", "-c", "echo -n $BASH_VERSION").Output()
	if err != nil {
		return false
	}
	return strings.HasPrefix(string(out), "5.1")
}

// concBuffer wraps a bytes.Buffer in a mutex so that concurrent writes
// to it don't upset the race detector.
type concBuffer struct {
	buf bytes.Buffer
	sync.Mutex
}

func absPath(dir, path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return filepath.Clean(path)
}

func (c *concBuffer) Write(p []byte) (int, error) {
	c.Lock()
	n, err := c.buf.Write(p)
	c.Unlock()
	return n, err
}

func (c *concBuffer) WriteString(s string) (int, error) {
	c.Lock()
	n, err := c.buf.WriteString(s)
	c.Unlock()
	return n, err
}

func (c *concBuffer) String() string {
	c.Lock()
	s := c.buf.String()
	c.Unlock()
	return s
}

func (c *concBuffer) Reset() {
	c.Lock()
	c.buf.Reset()
	c.Unlock()
}

type runTest struct {
	in, want string
}

var runTests = []runTest{
	// no-op programs
	{"", ""},
	{"true", ""},
	{":", ""},
	{"exit", ""},
	{"exit 0", ""},
	{"{ :; }", ""},
	{"(:)", ""},

	// exit status codes
	{"exit 1", "exit status 1"},
	{"exit -1", "exit status 255"},
	{"exit 300", "exit status 44"},
	{"false", "exit status 1"},
	{"false foo", "exit status 1"},
	{"! false", ""},
	{"true foo", ""},
	{": foo", ""},
	{"! true", "exit status 1"},
	{"false; true", ""},
	{"false; exit", "exit status 1"},
	{"exit; echo foo", ""},
	{"exit 0; echo foo", ""},
	{"printf", "usage: printf format [arguments]\nexit status 2 #JUSTERR"},
	{"break", "break is only useful in a loop #JUSTERR"},
	{"continue", "continue is only useful in a loop #JUSTERR"},
	{"cd a b", "usage: cd [dir]\nexit status 2 #JUSTERR"},
	{"shift a", "usage: shift [n]\nexit status 2 #JUSTERR"},
	{
		"shouldnotexist",
		"\"shouldnotexist\": executable file not found in $PATH\nexit status 127 #JUSTERR",
	},
	{
		"for i in 1; do continue a; done",
		"usage: continue [n]\nexit status 2 #JUSTERR",
	},
	{
		"for i in 1; do break a; done",
		"usage: break [n]\nexit status 2 #JUSTERR",
	},
	{"false; a=b", ""},
	{"false; false &", ""},

	// we don't need to follow bash error strings
	{"exit a", "invalid exit status code: \"a\"\nexit status 2 #JUSTERR"},
	{"exit 1 2", "exit cannot take multiple arguments\nexit status 1 #JUSTERR"},

	// echo
	{"echo", "\n"},
	{"echo a b c", "a b c\n"},
	{"echo -n foo", "foo"},
	{`echo -e '\t'`, "\t\n"},
	{`echo -E '\t'`, "\\t\n"},
	{"echo -x foo", "-x foo\n"},
	{"echo -e -x -e foo", "-x -e foo\n"},

	// printf
	{"printf foo", "foo"},
	{"printf %%", "%"},
	{"printf %", "missing format char\nexit status 1 #JUSTERR"},
	{"printf %; echo foo", "missing format char\nfoo\n #IGNORE"},
	{"printf %1", "missing format char\nexit status 1 #JUSTERR"},
	{"printf %+", "missing format char\nexit status 1 #JUSTERR"},
	{"printf %B foo", "invalid format char: B\nexit status 1 #JUSTERR"},
	{"printf %12-s foo", "invalid format char: -\nexit status 1 #JUSTERR"},
	{"printf ' %s \n' bar", " bar \n"},
	{"printf '\\A'", "\\A"},
	{"printf %s foo", "foo"},
	{"printf %s", ""},
	{"printf %d,%i 3 4", "3,4"},
	{"printf %d", "0"},
	{"printf %d,%d 010 0x10", "8,16"},
	{"printf %c,%c,%c foo àa", "f,\xc3,\x00"}, // TODO: use a rune?
	{"printf %3s a", "  a"},
	{"printf %3i 1", "  1"},
	{"printf %+i%+d 1 -3", "+1-3"},
	{"printf %-5x 10", "a    "},
	{"printf %02x 1", "01"},
	{"printf 'a% 5s' a", "a    a"},
	{"printf 'nofmt' 1 2 3", "nofmt"},
	{"printf '%d_' 1 2 3", "1_2_3_"},
	{"printf '%02d %02d\n' 1 2 3", "01 02\n03 00\n"},

	// words and quotes
	{"echo  foo ", "foo\n"},
	{"echo ' foo '", " foo \n"},
	{`echo " foo "`, " foo \n"},
	{`echo a'b'c"d"e`, "abcde\n"},
	{`a=" b c "; echo $a`, "b c\n"},
	{`a=" b c "; echo "$a"`, " b c \n"},
	{`echo "$(echo ' b c ')"`, " b c \n"},
	{"echo ''", "\n"},
	{`$(echo)`, ""},
	{`echo -n '\\'`, `\\`},
	{`echo -n "\\"`, `\`},
	{`set -- a b c; x="$@"; echo "$x"`, "a b c\n"},
	{`set -- b c; echo a"$@"d`, "ab cd\n"},
	{`count() { echo $#; }; set --; count "$@"`, "0\n"},
	{`count() { echo $#; }; set -- ""; count "$@"`, "1\n"},
	{`count() { echo $#; }; set -- ""; shift; count "$@"`, "0\n"},
	{`count() { echo $#; }; a=(); count "${a[@]}"`, "0\n"},
	{`count() { echo $#; }; a=(""); count "${a[@]}"`, "1\n"},
	{`echo $1 $3; set -- a b c; echo $1 $3`, "\na c\n"},
	{`[[ $0 == "bash" || $0 == "gosh" ]]`, ""},

	// dollar quotes
	{`echo $'foo\nbar'`, "foo\nbar\n"},
	{`echo $'\r\t\\'`, "\r\t\\\n"},
	{`echo $"foo\nbar"`, "foo\\nbar\n"},
	{`echo $'%s'`, "%s\n"},
	{`a=$'\r\t\\'; echo "$a"`, "\r\t\\\n"},
	{`a=$"foo\nbar"; echo "$a"`, "foo\\nbar\n"},
	{`echo $'\a\b\e\E\f\v'`, "\a\b\x1b\x1b\f\v\n"},
	{`echo $'\\\'\"\?'`, "\\'\"?\n"},
	{`echo $'\1\45\12345\777\9'`, "\x01%S45\xff\\9\n"},
	{`echo $'\x\xf\x09\xAB'`, "\\x\x0f\x09\xab\n"},
	{`echo $'\u\uf\u09\uABCD\u00051234'`, "\\u\u000f\u0009\uabcd\u00051234\n"},
	{`echo $'\U\Uf\U09\UABCD\U00051234'`, "\\U\u000f\u0009\uabcd\U00051234\n"},
	{
		"echo 'foo\x00bar'",
		"foobar\n",
	},
	{
		"echo \"foo\x00bar\"",
		"foobar\n",
	},
	{
		"echo $'foo\x00bar'",
		"foobar\n",
	},
	{
		"echo $'foo\\x00bar'",
		"foo\n",
	},
	{
		"echo $'foo\\xbar'",
		"foo\xbar\n",
	},
	{
		"a='foo\x00bar'; eval \"echo -n ${a} ${a@Q}\";",
		"foobar foobar",
	},
	{
		"a=$'foo\\x00bar'; eval \"echo -n ${a} ${a@Q}\";",
		"foo foo",
	},
	{
		"i\x00f true; then echo foo\x00; \x00fi",
		"foo\n",
	},
	{
		"echo $(GOSH_CMD=foo_null_bar $GOSH_PROG)",
		"foobar\n #IGNORE",
	},
	// See the TODO where FOO_NULL_BAR is set.
	// {
	// 	"echo $FOO_NULL_BAR \"${FOO_NULL_BAR}\"",
	// 	"foo\n",
	// },

	// escaped chars
	{"echo a\\b", "ab\n"},
	{"echo a\\ b", "a b\n"},
	{"echo \\$a", "$a\n"},
	{"echo \"a\\b\"", "a\\b\n"},
	{"echo 'a\\b'", "a\\b\n"},
	{"echo \"a\\\nb\"", "ab\n"},
	{"echo 'a\\\nb'", "a\\\nb\n"},
	{`echo "\""`, "\"\n"},
	{`echo \\`, "\\\n"},
	{`echo \\\\`, "\\\\\n"},
	{`echo \`, "\n"},

	// vars
	{"foo=bar; echo $foo", "bar\n"},
	{"foo=bar foo=etc; echo $foo", "etc\n"},
	{"foo=bar; foo=etc; echo $foo", "etc\n"},
	{"foo=bar; foo=; echo $foo", "\n"},
	{"unset foo; echo $foo", "\n"},
	{"foo=bar; unset foo; echo $foo", "\n"},
	{"echo $INTERP_GLOBAL", "value\n"},
	{"INTERP_GLOBAL=; echo $INTERP_GLOBAL", "\n"},
	{"unset INTERP_GLOBAL; echo $INTERP_GLOBAL", "\n"},
	{"echo $MIXEDCASE_INTERP_GLOBAL", "value\n"},
	{"foo=bar; foo=x true; echo $foo", "bar\n"},
	{"foo=bar; foo=x true; echo $foo", "bar\n"},
	{"foo=bar; $ENV_PROG | grep '^foo='", "exit status 1"},
	{"foo=bar $ENV_PROG | grep '^foo='", "foo=bar\n"},
	{"foo=a foo=b $ENV_PROG | grep '^foo='", "foo=b\n"},
	{"$ENV_PROG | grep '^INTERP_GLOBAL='", "INTERP_GLOBAL=value\n"},
	{"INTERP_GLOBAL=new; $ENV_PROG | grep '^INTERP_GLOBAL='", "INTERP_GLOBAL=new\n"},
	{"INTERP_GLOBAL=; $ENV_PROG | grep '^INTERP_GLOBAL='", "INTERP_GLOBAL=\n"},
	{"unset INTERP_GLOBAL; $ENV_PROG | grep '^INTERP_GLOBAL='", "exit status 1"},
	{"a=b; a+=c x+=y; echo $a $x", "bc y\n"},
	{`a=" x  y"; b=$a c="$a"; echo $b; echo $c`, "x y\nx y\n"},
	{`a=" x  y"; b=$a c="$a"; echo "$b"; echo "$c"`, " x  y\n x  y\n"},
	{`arr=("foo" "bar" "lala" "foobar"); echo ${arr[@]:2}; echo ${arr[*]:2}`, "lala foobar\nlala foobar\n"},
	{`arr=("foo" "bar" "lala" "foobar"); echo ${arr[@]:2:4}; echo ${arr[*]:1:4}`, "lala foobar\nbar lala foobar\n"},
	{`arr=("foo" "bar"); echo ${arr[@]}; echo ${arr[*]}`, "foo bar\nfoo bar\n"},
	{`arr=("foo"); echo ${arr[@]:99}`, "\n"},
	{`echo ${arr[@]:1:99}; echo ${arr[*]:1:99}`, "\n\n"},
	{`arr=(0 1 2 3 4 5 6 7 8 9 0 a b c d e f g h); echo ${arr[@]:3:4}`, "3 4 5 6\n"},
	{`echo ${foo[@]}; echo ${foo[*]}`, "\n\n"},
	// TODO: reenable once we figure out the broken pipe error
	//{`$ENV_PROG | while read line; do if test -z "$line"; then echo empty; fi; break; done`, ""}, // never begin with an empty element

	// inline variables have special scoping
	{
		"f() { echo $inline; inline=bar true; echo $inline; }; inline=foo f",
		"foo\nfoo\n",
	},
	{"v=x; read v <<< 'y'; echo $v", "y\n"},
	{"v=x; v=inline read v <<< 'y'; echo $v", "x\n"},
	{"v=x; v=inline unset v; echo $v", "x\n"},
	{"v=x; echo 'v=y' >f; v=inline source f; echo $v", "x\n"},
	{"declare -n v=v2; v=inline true; echo $v $v2", "\n"},
	{"f() { echo $v; }; v=x; v=y f; f", "y\nx\n"},
	{"f() { echo $v; }; v=x; v+=y f; f", "xy\nx\n"},
	{"f() { echo $v; }; declare -n v=v2; v2=x; v=y f; f", "y\nx\n"},
	{"f() { echo ${v[@]}; }; v=(e1 e2); v=y f; f", "y\ne1 e2\n"},

	// special vars
	{"echo $?; false; echo $?", "0\n1\n"},
	{"for i in 1 2; do\necho $LINENO\necho $LINENO\ndone", "2\n3\n2\n3\n"},
	{"[[ -n $$ && $$ -gt 0 ]]", ""},
	{"[[ $$ -eq $PPID ]]", "exit status 1"},

	// var manipulation
	{"echo ${#a} ${#a[@]}", "0 0\n"},
	{"a=bar; echo ${#a} ${#a[@]}", "3 1\n"},
	{"a=世界; echo ${#a}", "2\n"},
	{"a=(a bcd); echo ${#a} ${#a[@]} ${#a[*]} ${#a[1]}", "1 2 2 3\n"},
	{
		"a=($(echo a bcd)); echo ${#a} ${#a[@]} ${#a[*]} ${#a[1]}",
		"1 2 2 3\n",
	},
	{
		"a=([0]=$(echo a b) $(echo c d)); echo ${#a} ${#a[@]} ${#a[*]} ${#a[0]}",
		"3 3 3 3\n",
	},
	{"set -- a bc; echo ${#@} ${#*} $#", "2 2 2\n"},
	{
		"echo ${!a}; echo more",
		"invalid indirect expansion\nexit status 1 #JUSTERR",
	},
	{
		"a=b; echo ${!a}; b=c; echo ${!a}",
		"\nc\n",
	},
	{
		"a=foo; echo ${a:1}; echo ${a: -1}; echo ${a: -10}; echo ${a:5}",
		"oo\no\n\n\n",
	},
	{
		"a=foo; echo ${a::2}; echo ${a::-1}; echo ${a: -10}; echo ${a::5}",
		"fo\nfo\n\nfoo\n",
	},
	{
		"a=abc; echo ${a:1:1}",
		"b\n",
	},
	{
		"a=foo; echo ${a/no/x} ${a/o/i} ${a//o/i} ${a/fo/}",
		"foo fio fii o\n",
	},
	{
		"a=foo; echo ${a/*/xx} ${a//?/na} ${a/o*}",
		"xx nanana f\n",
	},
	{
		"a=12345; echo ${a//[42]} ${a//[^42]} ${a//[!42]}",
		"135 24 24\n",
	},
	{"a=0123456789; echo ${a//[1-35-8]}", "049\n"},
	{"a=]abc]; echo ${a//[]b]}", "ac\n"},
	{"a=-abc-; echo ${a//[-b]}", "ac\n"},
	{`a='x\y'; echo ${a//\\}`, "xy\n"},
	{"a=']'; echo ${a//[}", "]\n"},
	{"a=']'; echo ${a//[]}", "]\n"},
	{"a=']'; echo ${a//[]]}", "\n"},
	{"a='['; echo ${a//[[]}", "\n"},
	{"a=']'; echo ${a//[xy}", "]\n"},
	{"a='abc123'; echo ${a//[[:digit:]]}", "abc\n"},
	{"a='[[:wrong:]]'; echo ${a//[[:wrong:]]}", "[[:wrong:]]\n"},
	{"a='[[:wrong:]]'; echo ${a//[[:}", "[[:wrong:]]\n"},
	{"a='abcx1y'; echo ${a//x[[:digit:]]y}", "abc\n"},
	{`a=xyz; echo "${a/y/a  b}"`, "xa  bz\n"},
	{"a='foo/bar'; echo ${a//o*a/}", "fr\n"},
	{
		"echo ${a:-b}; echo $a; a=; echo ${a:-b}; a=c; echo ${a:-b}",
		"b\n\nb\nc\n",
	},
	{
		"echo ${#:-never} ${?:-never} ${LINENO:-never}",
		"0 0 1\n",
	},
	{
		"echo ${a-b}; echo $a; a=; echo ${a-b}; a=c; echo ${a-b}",
		"b\n\n\nc\n",
	},
	{
		"echo ${a:=b}; echo $a; a=; echo ${a:=b}; a=c; echo ${a:=b}",
		"b\nb\nb\nc\n",
	},
	{
		"echo ${a=b}; echo $a; a=; echo ${a=b}; a=c; echo ${a=b}",
		"b\nb\n\nc\n",
	},
	{
		"echo ${a:+b}; echo $a; a=; echo ${a:+b}; a=c; echo ${a:+b}",
		"\n\n\nb\n",
	},
	{
		"echo ${a+b}; echo $a; a=; echo ${a+b}; a=c; echo ${a+b}",
		"\n\nb\nb\n",
	},
	{
		"a=b; echo ${a:?err1}; a=; echo ${a:?err2}; unset a; echo ${a:?err3}",
		"b\na: err2\nexit status 1 #JUSTERR",
	},
	{
		"a=b; echo ${a?err1}; a=; echo ${a?err2}; unset a; echo ${a?err3}",
		"b\n\na: err3\nexit status 1 #JUSTERR",
	},
	{
		"echo ${a:?%s}",
		"a: %s\nexit status 1 #JUSTERR",
	},
	{
		"x=aaabccc; echo ${x#*a}; echo ${x##*a}",
		"aabccc\nbccc\n",
	},
	{
		"x=(__a _b c_); echo ${x[@]#_}",
		"_a b c_\n",
	},
	{
		"x=(a__ b_ _c); echo ${x[@]%%_}",
		"a_ b _c\n",
	},
	{
		"x=aaabccc; echo ${x%c*}; echo ${x%%c*}",
		"aaabcc\naaab\n",
	},
	{
		"x=aaabccc; echo ${x%%[bc}",
		"aaabccc\n",
	},
	{
		"a='àÉñ bAr'; echo ${a^}; echo ${a^^}",
		"ÀÉñ bAr\nÀÉÑ BAR\n",
	},
	{
		"a='àÉñ bAr'; echo ${a,}; echo ${a,,}",
		"àÉñ bAr\nàéñ bar\n",
	},
	{
		"a='àÉñ bAr'; echo ${a^?}; echo ${a^^[br]}",
		"ÀÉñ bAr\nàÉñ BAR\n",
	},
	{
		"a='àÉñ bAr'; echo ${a,?}; echo ${a,,[br]}",
		"àÉñ bAr\nàÉñ bAr\n",
	},
	{
		"a=(àÉñ bAr); echo ${a[@]^}; echo ${a[*],,}",
		"ÀÉñ BAr\nàéñ bar\n",
	},
	{
		"INTERP_X_1=a INTERP_X_2=b; echo ${!INTERP_X_*}",
		"INTERP_X_1 INTERP_X_2\n",
	},
	{
		"INTERP_X_2=b INTERP_X_1=a; echo ${!INTERP_*}",
		"INTERP_GLOBAL INTERP_X_1 INTERP_X_2\n",
	},
	{
		`INTERP_X_2=b INTERP_X_1=a; set -- ${!INTERP_*}; echo $#`,
		"3\n",
	},
	{
		`INTERP_X_2=b INTERP_X_1=a; set -- "${!INTERP_*}"; echo $#`,
		"1\n",
	},
	{
		`INTERP_X_2=b INTERP_X_1=a; set -- ${!INTERP_@}; echo $#`,
		"3\n",
	},
	{
		`INTERP_X_2=b INTERP_X_1=a; set -- "${!INTERP_@}"; echo $#`,
		"3\n",
	},
	{
		`a='b  c'; eval "echo -n ${a} ${a@Q}"`,
		`b c b  c`,
	},
	{
		`a='"\n'; printf "%s %s" "${a}" "${a@E}"`,
		"\"\\n \"\n",
	},
	{
		"declare a; a+=(b); echo ${a[@]} ${#a[@]}",
		"b 1\n",
	},
	{
		`a=""; a+=(b); echo ${a[@]} ${#a[@]}`,
		"b 2\n",
	},
	{
		"f() { local a; a=bad; a=good; echo $a; }; f",
		"good\n",
	},

	// if
	{
		"if true; then echo foo; fi",
		"foo\n",
	},
	{
		"if false; then echo foo; fi",
		"",
	},
	{
		"if false; then echo foo; fi",
		"",
	},
	{
		"if true; then echo foo; else echo bar; fi",
		"foo\n",
	},
	{
		"if false; then echo foo; else echo bar; fi",
		"bar\n",
	},
	{
		"if true; then false; fi",
		"exit status 1",
	},
	{
		"if false; then :; else false; fi",
		"exit status 1",
	},
	{
		"if false; then :; elif true; then echo foo; fi",
		"foo\n",
	},
	{
		"if false; then :; elif false; then :; elif true; then echo foo; fi",
		"foo\n",
	},
	{
		"if false; then :; elif false; then :; else echo foo; fi",
		"foo\n",
	},

	// while
	{
		"while false; do echo foo; done",
		"",
	},
	{
		"while true; do exit 1; done",
		"exit status 1",
	},
	{
		"while true; do break; done",
		"",
	},
	{
		"while true; do while true; do break 2; done; done",
		"",
	},

	// until
	{
		"until true; do echo foo; done",
		"",
	},
	{
		"until false; do exit 1; done",
		"exit status 1",
	},
	{
		"until false; do break; done",
		"",
	},

	// for
	{
		"for i in 1 2 3; do echo $i; done",
		"1\n2\n3\n",
	},
	{
		"for i in 1 2 3; do echo $i; exit; done",
		"1\n",
	},
	{
		"for i in 1 2 3; do echo $i; false; done",
		"1\n2\n3\nexit status 1",
	},
	{
		"for i in 1 2 3; do echo $i; break; done",
		"1\n",
	},
	{
		"for i in 1 2 3; do echo $i; continue; echo foo; done",
		"1\n2\n3\n",
	},
	{
		"for i in 1 2; do for j in a b; do echo $i $j; continue 2; done; done",
		"1 a\n2 a\n",
	},
	{
		"for ((i=0; i<3; i++)); do echo $i; done",
		"0\n1\n2\n",
	},
	// for, with missing Init, Cond, Post
	{
		"i=0; for ((; i<3; i++)); do echo $i; done",
		"0\n1\n2\n",
	},
	{
		"for ((i=0;; i++)); do if [ $i -ge 3 ]; then break; fi; echo $i; done",
		"0\n1\n2\n",
	},
	{
		"for ((i=0; i<3;)); do echo $i; i=$((i+1)); done",
		"0\n1\n2\n",
	},
	{
		"i=0; for ((;;)); do if [ $i -ge 3 ]; then break; fi; echo $i; i=$((i+1)); done",
		"0\n1\n2\n",
	},
	// TODO: uncomment once expandEnv.Set starts returning errors
	// {
	// 	"readonly i; for ((i=0; i<3; i++)); do echo $i; done",
	// 	"0\n1\n2\n",
	// },
	{
		"for ((i=5; i>0; i--)); do echo $i; break; done",
		"5\n",
	},
	{
		"for i in 1 2; do for j in a b; do echo $i $j; done; break; done",
		"1 a\n1 b\n",
	},
	{
		"for i in 1 2 3; do :; done; echo $i",
		"3\n",
	},
	{
		"for ((i=0; i<3; i++)); do :; done; echo $i",
		"3\n",
	},
	{
		"set -- a 'b c'; for i in; do echo $i; done",
		"",
	},
	{
		"set -- a 'b c'; for i; do echo $i; done",
		"a\nb c\n",
	},

	// block
	{
		"{ echo foo; }",
		"foo\n",
	},
	{
		"{ false; }",
		"exit status 1",
	},

	// subshell
	{
		"(echo foo)",
		"foo\n",
	},
	{
		"(false)",
		"exit status 1",
	},
	{
		"(exit 1)",
		"exit status 1",
	},
	{
		"(foo=bar; echo $foo); echo $foo",
		"bar\n\n",
	},
	{
		"(echo() { printf 'bar\n'; }; echo); echo",
		"bar\n\n",
	},
	{
		"unset INTERP_GLOBAL & echo $INTERP_GLOBAL",
		"value\n",
	},
	{
		"(fn() { :; }) & pwd >/dev/null",
		"",
	},
	{
		"x[0]=x; (echo ${x[0]}; x[0]=y; echo ${x[0]}); echo ${x[0]}",
		"x\ny\nx\n",
	},
	{
		`x[3]=x; (x[3]=y); echo ${x[3]}`,
		"x\n",
	},
	{
		"shopt -s expand_aliases; alias f='echo x'\nf\n(f\nalias f='echo y'\neval f\n)\nf\n",
		"x\nx\ny\nx\n",
	},
	{
		"set -- a; echo $1; (echo $1; set -- b; echo $1); echo $1",
		"a\na\nb\na\n",
	},
	{"false; ( echo $? )", "1\n"},

	// cd/pwd
	{"[[ fo~ == 'fo~' ]]", ""},
	{`[[ 'ab\c' == *\\* ]]`, ""},
	{`[[ foo/bar == foo* ]]`, ""},
	{"[[ a == [ab ]]", "exit status 1"},
	{`HOME='/*'; echo ~; echo "$HOME"`, "/*\n/*\n"},
	{`test -d ~`, ""},
	{`foo=~; test -d $foo`, ""},
	{`foo=~; test -d "$foo"`, ""},
	{`foo='~'; test -d $foo`, "exit status 1"},
	{`foo='~'; [ $foo == '~' ]`, ""},
	{
		`[[ ~ == "$HOME" ]] && [[ ~/foo == "$HOME/foo" ]]`,
		"",
	},
	{
		`HOME=$PWD/home; mkdir home; touch home/f; [[ -e ~/f ]]`,
		"",
	},
	{
		`HOME=$PWD/home; mkdir home; touch home/f; [[ ~/f -ef $HOME/f ]]`,
		"",
	},
	{
		"[[ ~noexist == '~noexist' ]]",
		"",
	},
	{
		`w="$HOME"; cd; [[ $PWD == "$w" ]]`,
		"",
	},
	{
		`HOME=/foo; echo $HOME`,
		"/foo\n",
	},
	{
		"cd noexist",
		"exit status 1 #JUSTERR",
	},
	{
		"mkdir -p a/b && cd a && cd b && cd ../..",
		"",
	},
	{
		">a && cd a",
		"exit status 1 #JUSTERR",
	},
	{
		`[[ $PWD == "$(pwd)" ]]`,
		"",
	},
	{
		"PWD=changed; [[ $PWD == changed ]]",
		"",
	},
	{
		"PWD=changed; mkdir a; cd a; [[ $PWD == changed ]]",
		"exit status 1",
	},
	{
		`mkdir %s; old="$PWD"; cd %s; [[ $old == "$PWD" ]]`,
		"exit status 1",
	},
	{
		`old="$PWD"; mkdir a; cd a; cd ..; [[ $old == "$PWD" ]]`,
		"",
	},
	{
		`[[ $PWD == "$OLDPWD" ]]`,
		"exit status 1",
	},
	{
		`old="$PWD"; mkdir a; cd a; [[ $old == "$OLDPWD" ]]`,
		"",
	},
	{
		`mkdir a; ln -s a b; [[ $(cd a && pwd) == "$(cd b && pwd)" ]]; echo $?`,
		"1\n",
	},
	{
		`pwd -a`,
		"invalid option: \"-a\"\nexit status 2 #JUSTERR",
	},
	{
		`pwd -L -P -a`,
		"invalid option: \"-a\"\nexit status 2 #JUSTERR",
	},
	{
		`mkdir a; ln -s a b; [[ "$(cd a && pwd -P)" == "$(cd b && pwd -P)" ]]`,
		"",
	},
	{
		`mkdir a; ln -s a b; [[ "$(cd a && pwd -P)" == "$(cd b && pwd -L)" ]]; echo $?`,
		"1\n",
	},
	{
		`orig="$PWD"; mkdir a; cd a; cd -; [[ "$PWD" == "$orig" ]]`,
		"",
	},

	// dirs/pushd/popd
	{"set -- $(dirs); echo $# ${#DIRSTACK[@]}", "1 1\n"},
	{"pushd", "pushd: no other directory\nexit status 1 #JUSTERR"},
	{"pushd -n", ""},
	{"pushd foo bar", "pushd: too many arguments\nexit status 2 #JUSTERR"},
	{"pushd does-not-exist; set -- $(dirs); echo $#", "1\n #IGNORE"},
	{"mkdir a; pushd a >/dev/null; set -- $(dirs); echo $#", "2\n"},
	{"mkdir a; set -- $(pushd a); echo $#", "2\n"},
	{
		`mkdir a; pushd a >/dev/null; set -- $(dirs); [[ $1 == "$HOME" ]]`,
		"exit status 1",
	},
	{
		`mkdir a; pushd a >/dev/null; [[ ${DIRSTACK[0]} == "$HOME" ]]`,
		"exit status 1",
	},
	{
		`old=$(dirs); mkdir a; pushd a >/dev/null; pushd >/dev/null; set -- $(dirs); [[ $1 == "$old" ]]`,
		"",
	},
	{
		`old=$(dirs); mkdir a; pushd a >/dev/null; pushd -n >/dev/null; set -- $(dirs); [[ $1 == "$old" ]]`,
		"exit status 1",
	},
	{
		"mkdir a; pushd a >/dev/null; pushd >/dev/null; rm -r a; pushd",
		"exit status 1 #JUSTERR",
	},
	{
		`old=$(dirs); mkdir a; pushd -n a >/dev/null; set -- $(dirs); [[ $1 == "$old" ]]`,
		"",
	},
	{
		`old=$(dirs); mkdir a; pushd -n a >/dev/null; pushd >/dev/null; set -- $(dirs); [[ $1 == "$old" ]]`,
		"exit status 1",
	},
	{"popd", "popd: directory stack empty\nexit status 1 #JUSTERR"},
	{"popd -n", "popd: directory stack empty\nexit status 1 #JUSTERR"},
	{"popd foo", "popd: invalid argument\nexit status 2 #JUSTERR"},
	{"old=$(dirs); mkdir a; pushd a >/dev/null; set -- $(popd); echo $#", "1\n"},
	{
		`old=$(dirs); mkdir a; pushd a >/dev/null; popd >/dev/null; [[ $(dirs) == "$old" ]]`,
		"",
	},
	{"old=$(dirs); mkdir a; pushd a >/dev/null; set -- $(popd -n); echo $#", "1\n"},
	{
		`old=$(dirs); mkdir a; pushd a >/dev/null; popd -n >/dev/null; [[ $(dirs) == "$old" ]]`,
		"exit status 1",
	},
	{
		"mkdir a; pushd a >/dev/null; pushd >/dev/null; rm -r a; popd",
		"exit status 1 #JUSTERR",
	},

	// binary cmd
	{
		"true && echo foo || echo bar",
		"foo\n",
	},
	{
		"false && echo foo || echo bar",
		"bar\n",
	},

	// func
	{
		"foo() { echo bar; }; foo",
		"bar\n",
	},
	{
		"foo() { echo $1; }; foo",
		"\n",
	},
	{
		"foo() { echo $1; }; foo a b",
		"a\n",
	},
	{
		"foo() { echo $1; bar c d; echo $2; }; bar() { echo $2; }; foo a b",
		"a\nd\nb\n",
	},
	{
		`foo() { echo $#; }; foo; foo 1 2 3; foo "a b"; echo $#`,
		"0\n3\n1\n0\n",
	},
	{
		`foo() { for a in $*; do echo "$a"; done }; foo 'a  1' 'b  2'`,
		"a\n1\nb\n2\n",
	},
	{
		`foo() { for a in "$*"; do echo "$a"; done }; foo 'a  1' 'b  2'`,
		"a  1 b  2\n",
	},
	{
		`foo() { for a in "foo$*"; do echo "$a"; done }; foo 'a  1' 'b  2'`,
		"fooa  1 b  2\n",
	},
	{
		`foo() { for a in $@; do echo "$a"; done }; foo 'a  1' 'b  2'`,
		"a\n1\nb\n2\n",
	},
	{
		`foo() { for a in "$@"; do echo "$a"; done }; foo 'a  1' 'b  2'`,
		"a  1\nb  2\n",
	},

	// alias (note the input newlines)
	{
		"alias foo; alias foo=echo; alias foo; alias foo=; alias foo",
		"alias: \"foo\" not found\nalias foo='echo'\nalias foo=''\n #IGNORE",
	},
	{
		"shopt -s expand_aliases; alias foo=echo\nfoo foo; foo bar",
		"foo\nbar\n",
	},
	{
		"shopt -s expand_aliases; alias true=echo\ntrue foo; unalias true\ntrue bar",
		"foo\n",
	},
	{
		"shopt -s expand_aliases; alias echo='echo a'\necho b c",
		"a b c\n",
	},
	{
		"shopt -s expand_aliases; alias foo='echo '\nfoo foo; foo bar",
		"echo\nbar\n",
	},

	// case
	{
		"case b in x) echo foo ;; a|b) echo bar ;; esac",
		"bar\n",
	},
	{
		"case b in x) echo foo ;; y|z) echo bar ;; esac",
		"",
	},
	{
		"case foo in bar) echo foo ;; *) echo bar ;; esac",
		"bar\n",
	},
	{
		"case foo in *o*) echo bar ;; esac",
		"bar\n",
	},
	{
		"case foo in '*') echo x ;; f*) echo y ;; esac",
		"y\n",
	},

	// exec
	{
		"$GOSH_PROG 'echo foo'",
		"foo\n",
	},
	{
		"$GOSH_PROG 'echo foo >&2' >/dev/null",
		"foo\n",
	},
	{
		"echo foo | $GOSH_PROG 'cat >&2' >/dev/null",
		"foo\n",
	},
	{
		"$GOSH_PROG 'exit 1'",
		"exit status 1",
	},
	{
		"exec >/dev/null; echo foo",
		"",
	},

	// return
	{"return", "return: can only be done from a func or sourced script\nexit status 1 #JUSTERR"},
	{"f() { return; }; f", ""},
	{"f() { return 2; }; f", "exit status 2"},
	{"f() { echo foo; return; echo bar; }; f", "foo\n"},
	{"f1() { :; }; f2() { f1; return; }; f2", ""},
	{"echo 'return' >a; source a", ""},
	{"echo 'return' >a; source a; return", "return: can only be done from a func or sourced script\nexit status 1 #JUSTERR"},
	{"echo 'return 2' >a; source a", "exit status 2"},
	{"echo 'echo foo; return; echo bar' >a; source a", "foo\n"},

	// command
	{"command", ""},
	{"command -o echo", "command: invalid option \"-o\"\nexit status 2 #JUSTERR"},
	{"command -vo echo", "command: invalid option \"-o\"\nexit status 2 #JUSTERR"},
	{"echo() { :; }; echo foo", ""},
	{"echo() { :; }; command echo foo", "foo\n"},
	{"command -v does-not-exist", "exit status 1"},
	{"foo() { :; }; command -v foo", "foo\n"},
	{"foo() { :; }; command -v does-not-exist foo", "foo\n"},
	{"command -v echo", "echo\n"},
	{"[[ $(command -v $PATH_PROG) == $PATH_PROG ]]", "exit status 1"},

	// cmd substitution
	{
		"echo foo $(printf bar)",
		"foo bar\n",
	},
	{
		"echo foo $(echo bar)",
		"foo bar\n",
	},
	{
		"$(echo echo foo bar)",
		"foo bar\n",
	},
	{
		"for i in 1 $(echo 2 3) 4; do echo $i; done",
		"1\n2\n3\n4\n",
	},
	{
		"echo 1$(echo 2 3)4",
		"12 34\n",
	},
	{
		`mkdir d; [[ $(cd d && pwd) == "$(pwd)" ]]`,
		"exit status 1",
	},
	{
		"a=sub true & { a=main $ENV_PROG | grep '^a='; }",
		"a=main\n",
	},
	{
		"echo foo >f; echo $(cat f); echo $(<f)",
		"foo\nfoo\n",
	},
	{
		"echo foo >f; echo $(<f; echo bar)",
		"bar\n",
	},

	// pipes
	{
		"echo foo | sed 's/o/a/g'",
		"faa\n",
	},
	{
		"echo foo | false | true",
		"",
	},
	{
		"true $(true) | true", // used to panic
		"",
	},
	{
		// The first command in the block used to consume stdin, even
		// though it shouldn't be. We just want to run any arbitrary
		// non-builtin program that doesn't consume stdin.
		"echo foo | { $ENV_PROG >/dev/null; cat; }",
		"foo\n",
	},

	// redirects
	{
		"echo foo >&1 | sed 's/o/a/g'",
		"faa\n",
	},
	{
		"echo foo >&2 | sed 's/o/a/g'",
		"foo\n",
	},
	{
		// TODO: why does bash need a block here?
		"{ echo foo >&2; } |& sed 's/o/a/g'",
		"faa\n",
	},
	{
		"echo foo >/dev/null; echo bar",
		"bar\n",
	},
	{
		">a; echo foo >>b; wc -c <a >>b; cat b | tr -d ' '",
		"foo\n0\n",
	},
	{
		"echo foo >a; <a",
		"",
	},
	{
		"echo foo >a; wc -c <a | tr -d ' '",
		"4\n",
	},
	{
		"echo foo >>a; echo bar &>>a; wc -c <a | tr -d ' '",
		"8\n",
	},
	{
		"{ echo a; echo b >&2; } &>/dev/null",
		"",
	},
	{
		"sed 's/o/a/g' <<EOF\nfoo$foo\nEOF",
		"faa\n",
	},
	{
		"sed 's/o/a/g' <<'EOF'\nfoo$foo\nEOF",
		"faa$faa\n",
	},
	{
		"sed 's/o/a/g' <<EOF\n\tfoo\nEOF",
		"\tfaa\n",
	},
	{
		"sed 's/o/a/g' <<EOF\nfoo\nEOF",
		"faa\n",
	},
	{
		"cat <<EOF\n~/foo\nEOF",
		"~/foo\n",
	},
	{
		"sed 's/o/a/g' <<<foo$foo",
		"faa\n",
	},
	{
		"cat <<-EOF\n\tfoo\nEOF",
		"foo\n",
	},
	{
		"cat <<-EOF\n\tfoo\n\nEOF",
		"foo\n\n",
	},
	{
		"cat <<EOF\nfoo\\\nbar\nEOF",
		"foobar\n",
	},
	{
		"cat <<'EOF'\nfoo\\\nbar\nEOF",
		"foo\\\nbar\n",
	},
	{
		"mkdir a; echo foo >a |& grep -q 'is a directory'",
		" #IGNORE bash prints a warning",
	},
	{
		"echo foo 1>&1 | sed 's/o/a/g'",
		"faa\n",
	},
	{
		"echo foo 2>&2 |& sed 's/o/a/g'",
		"faa\n",
	},
	{
		"printf 2>&1 | sed 's/.*usage.*/foo/'",
		"foo\n",
	},
	{
		"mkdir a && cd a && echo foo >b && cd .. && cat a/b",
		"foo\n",
	},

	// background/wait
	{"wait", ""},
	{"{ true; } & wait", ""},
	{"{ exit 1; } & wait", ""},
	{
		"{ echo foo; } & wait; echo bar",
		"foo\nbar\n",
	},
	{
		"{ echo foo & wait; } & wait; echo bar",
		"foo\nbar\n",
	},
	{`mkdir d; old=$PWD; cd d & wait; [[ $old == "$PWD" ]]`, ""},
	{
		"f() { echo 1; }; { sleep 0.01s; f; } & f() { echo 2; }; wait",
		"1\n",
	},

	// bash test
	{
		"[[ a ]]",
		"",
	},
	{
		"[[ '' ]]",
		"exit status 1",
	},
	{
		"[[ '' ]]; [[ a ]]",
		"",
	},
	{
		"[[ ! (a == b) ]]",
		"",
	},
	{
		"[[ a != b ]]",
		"",
	},
	{
		"[[ a && '' ]]",
		"exit status 1",
	},
	{
		"[[ a || '' ]]",
		"",
	},
	{
		"[[ a > 3 ]]",
		"",
	},
	{
		"[[ a < 3 ]]",
		"exit status 1",
	},
	{
		"[[ 3 == 03 ]]",
		"exit status 1",
	},
	{
		"[[ a -eq b ]]",
		"",
	},
	{
		"[[ 3 -eq 03 ]]",
		"",
	},
	{
		"[[ 3 -ne 4 ]]",
		"",
	},
	{
		"[[ 3 -le 4 ]]",
		"",
	},
	{
		"[[ 3 -ge 4 ]]",
		"exit status 1",
	},
	{
		"[[ 3 -ge 3 ]]",
		"",
	},
	{
		"[[ 3 -lt 4 ]]",
		"",
	},
	{
		"[[ 3 -gt 4 ]]",
		"exit status 1",
	},
	{
		"[[ 3 -gt 3 ]]",
		"exit status 1",
	},
	{
		"[[ a -nt a || a -ot a ]]",
		"exit status 1",
	},
	{
		"touch -t 202111050000 a b; [[ a -nt b || a -ot b ]]",
		"exit status 1",
	},
	{
		"touch -t 202111050000 a; touch -t 202111060000 b; [[ a -nt b ]]",
		"exit status 1",
	},
	{
		"touch -t 202111050000 a; touch -t 202111060000 b; [[ a -ot b ]]",
		"",
	},
	{
		"[[ a -ef b ]]",
		"exit status 1",
	},
	{
		">a >b; [[ a -ef b ]]",
		"exit status 1",
	},
	{
		">a; [[ a -ef a ]]",
		"",
	},
	{
		">a; ln a b; [[ a -ef b ]]",
		"",
	},
	{
		">a; ln -s a b; [[ a -ef b ]]",
		"",
	},
	{
		"[[ -z 'foo' || -n '' ]]",
		"exit status 1",
	},
	{
		"[[ -z '' && -n 'foo' ]]",
		"",
	},
	{
		"a=x b=''; [[ -v a && -v b && ! -v c ]]",
		"",
	},
	{
		"[[ abc == *b* ]]",
		"",
	},
	{
		"[[ abc != *b* ]]",
		"exit status 1",
	},
	{
		"[[ *b = '*b' ]]",
		"",
	},
	{
		"[[ ab == a. ]]",
		"exit status 1",
	},
	{
		`x='*b*'; [[ abc == $x ]]`,
		"",
	},
	{
		`x='*b*'; [[ abc == "$x" ]]`,
		"exit status 1",
	},
	{
		`[[ abc == \a\bc ]]`,
		"",
	},
	{
		"[[ abc != *b'*' ]]",
		"",
	},
	{
		"[[ a =~ b ]]",
		"exit status 1",
	},
	{
		"[[ foo =~ foo && foo =~ .* && foo =~ f.o ]]",
		"",
	},
	{
		"[[ foo =~ oo ]] && echo foo; [[ foo =~ ^oo$ ]] && echo bar || true",
		"foo\n",
	},
	{
		"[[ a =~ [ ]]",
		"exit status 2",
	},
	{
		"[[ -e a ]] && echo x; >a; [[ -e a ]] && echo y",
		"y\n",
	},
	{
		"ln -s b a; [[ -e a ]] && echo x; >b; [[ -e a ]] && echo y",
		"y\n",
	},
	{
		"[[ -f a ]] && echo x; >a; [[ -f a ]] && echo y",
		"y\n",
	},
	{
		"[[ -e a ]] && echo x; mkdir a; [[ -e a ]] && echo y",
		"y\n",
	},
	{
		"[[ -d a ]] && echo x; mkdir a; [[ -d a ]] && echo y",
		"y\n",
	},
	{
		"[[ -r a ]] && echo x; >a; [[ -r a ]] && echo y",
		"y\n",
	},
	{
		"[[ -w a ]] && echo x; >a; [[ -w a ]] && echo y",
		"y\n",
	},
	{
		"[[ -s a ]] && echo x; echo body >a; [[ -s a ]] && echo y",
		"y\n",
	},
	{
		"[[ -L a ]] && echo x; ln -s b a; [[ -L a ]] && echo y;",
		"y\n",
	},
	{
		"mkdir a; cd a; test -f b && echo x; >b; test -f b && echo y",
		"y\n",
	},
	{
		">a; [[ -b a ]] && echo block; [[ -c a ]] && echo char; true",
		"",
	},
	{
		"[[ -e /dev/sda ]] || { echo block; exit; }; [[ -b /dev/sda ]] && echo block; [[ -c /dev/sda ]] && echo char; true",
		"block\n",
	},
	{
		"[[ -e /dev/nvme0n1 ]] || { echo block; exit; }; [[ -b /dev/nvme0n1 ]] && echo block; [[ -c /dev/nvme0n1 ]] && echo char; true",
		"block\n",
	},
	{
		"[[ -e /dev/tty ]] || { echo char; exit; }; [[ -b /dev/tty ]] && echo block; [[ -c /dev/tty ]] && echo char; true",
		"char\n",
	},
	{"[[ -t 1 ]]", "exit status 1"},
	{"[[ -t 1234 ]]", "exit status 1"},
	{"[[ -o wrong ]]", "exit status 1"},
	{"[[ -o errexit ]]", "exit status 1"},
	{"set -e; [[ -o errexit ]]", ""},
	{"[[ -o noglob ]]", "exit status 1"},
	{"set -f; [[ -o noglob ]]", ""},
	{"[[ -o allexport ]]", "exit status 1"},
	{"set -a; [[ -o allexport ]]", ""},
	{"[[ -o nounset ]]", "exit status 1"},
	{"set -u; [[ -o nounset ]]", ""},
	{"[[ -o noexec ]]", "exit status 1"},
	{"set -n; [[ -o noexec ]]", ""}, // actually does nothing, but oh well
	{"[[ -o pipefail ]]", "exit status 1"},
	{"set -o pipefail; [[ -o pipefail ]]", ""},

	// classic test
	{
		"[",
		"1:1: [: missing matching ]\nexit status 2 #JUSTERR",
	},
	{
		"[ a",
		"1:1: [: missing matching ]\nexit status 2 #JUSTERR",
	},
	{
		"[ a b c ]",
		"1:1: not a valid test operator: b\nexit status 2 #JUSTERR",
	},
	{
		"[ a -a ]",
		"1:1: -a must be followed by an expression\nexit status 2 #JUSTERR",
	},
	{"[ a ]", ""},
	{"[ -n ]", ""},
	{"[ '-n' ]", ""},
	{"[ -z ]", ""},
	{"[ ! ]", ""},
	{"[ a != b ]", ""},
	{"[ ! a '==' a ]", "exit status 1"},
	{"[ a -a 0 -gt 1 ]", "exit status 1"},
	{"[ 0 -gt 1 -o 1 -gt 0 ]", ""},
	{"[ 3 -gt 4 ]", "exit status 1"},
	{"[ 3 -lt 4 ]", ""},
	{
		"[ -e a ] && echo x; >a; [ -e a ] && echo y",
		"y\n",
	},
	{
		"test 3 -gt 4",
		"exit status 1",
	},
	{
		"test 3 -lt 4",
		"",
	},
	{
		"test 3 -lt",
		"1:1: -lt must be followed by a word\nexit status 2 #JUSTERR",
	},
	{
		"touch -t 202111050000 a; touch -t 202111060000 b; [ a -nt b ]",
		"exit status 1",
	},
	{
		"touch -t 202111050000 a; touch -t 202111060000 b; [ a -ot b ]",
		"",
	},
	{
		">a; [ a -ef a ]",
		"",
	},
	{"[ 3 -eq 04 ]", "exit status 1"},
	{"[ 3 -eq 03 ]", ""},
	{"[ 3 -ne 03 ]", "exit status 1"},
	{"[ 3 -le 4 ]", ""},
	{"[ 3 -ge 4 ]", "exit status 1"},
	{
		"[ -d a ] && echo x; mkdir a; [ -d a ] && echo y",
		"y\n",
	},
	{
		"[ -r a ] && echo x; >a; [ -r a ] && echo y",
		"y\n",
	},
	{
		"[ -w a ] && echo x; >a; [ -w a ] && echo y",
		"y\n",
	},
	{
		"[ -s a ] && echo x; echo body >a; [ -s a ] && echo y",
		"y\n",
	},
	{
		"[ -L a ] && echo x; ln -s b a; [ -L a ] && echo y;",
		"y\n",
	},
	{
		">a; [ -b a ] && echo block; [ -c a ] && echo char; true",
		"",
	},
	{"[ -t 1 ]", "exit status 1"},
	{"[ -t 1234 ]", "exit status 1"},
	{"[ -o wrong ]", "exit status 1"},
	{"[ -o errexit ]", "exit status 1"},
	{"set -e; [ -o errexit ]", ""},
	{"a=x b=''; [ -v a -a -v b -a ! -v c ]", ""},
	{"[ a = a ]", ""},
	{"[ a != a ]", "exit status 1"},
	{"[ abc = ab* ]", "exit status 1"},
	{"[ abc != ab* ]", ""},

	// arithm
	{
		"echo $((1 == +1))",
		"1\n",
	},
	{
		"echo $((!0))",
		"1\n",
	},
	{
		"echo $((!3))",
		"0\n",
	},
	{
		"echo $((~0))",
		"-1\n",
	},
	{
		"echo $((~3))",
		"-4\n",
	},
	{
		"echo $((1 + 2 - 3))",
		"0\n",
	},
	{
		"echo $((-1 * 6 / 2))",
		"-3\n",
	},
	{
		"a=2; echo $(( a + $a + c ))",
		"4\n",
	},
	{
		"a=b; b=c; c=5; echo $((a % 3))",
		"2\n",
	},
	{
		"echo $((2 > 2 || 2 < 2))",
		"0\n",
	},
	{
		"echo $((2 >= 2 && 2 <= 2))",
		"1\n",
	},
	{
		"echo $(((1 & 2) != (1 | 2)))",
		"1\n",
	},
	{
		"echo $a; echo $((a = 3 ^ 2)); echo $a",
		"\n1\n1\n",
	},
	{
		"echo $((a += 1, a *= 2, a <<= 2, a >> 1))",
		"4\n",
	},
	{
		"echo $((a -= 10, a /= 2, a >>= 1, a << 1))",
		"-6\n",
	},
	{
		"echo $((a |= 3, a &= 1, a ^= 8, a %= 5, a))",
		"4\n",
	},
	{
		"echo $((a = 3, ++a, a--))",
		"4\n",
	},
	{
		"echo $((2 ** 3)) $((1234 ** 4567))",
		"8 0\n",
	},
	{
		"echo $((1 ? 2 : 3)) $((0 ? 2 : 3))",
		"2 3\n",
	},
	{
		"((1))",
		"",
	},
	{
		"((3 == 4))",
		"exit status 1",
	},
	{
		"let i=(3+4); let i++; echo $i; let i--; echo $i",
		"8\n7\n",
	},
	{
		"let 3==4",
		"exit status 1",
	},
	{
		"a=1; let a++; echo $a",
		"2\n",
	},
	{
		"a=$((1 + 2)); echo $a",
		"3\n",
	},
	{
		"x=3; echo $(($x)) $((x))",
		"3 3\n",
	},
	{
		"set -- 1; echo $(($@))",
		"1\n",
	},
	{
		"a=b b=a; echo $(($a))",
		"0\n #IGNORE bash prints a warning",
	},

	// set/shift
	{
		"echo $#; set foo bar; echo $#",
		"0\n2\n",
	},
	{
		"shift; set a b c; shift; echo $@",
		"b c\n",
	},
	{
		"shift 2; set a b c; shift 2; echo $@",
		"c\n",
	},
	{
		`echo $#; set '' ""; echo $#`,
		"0\n2\n",
	},
	{
		"set -- a b; echo $#",
		"2\n",
	},
	{
		"set -U",
		"set: invalid option: \"-U\"\nexit status 2 #JUSTERR",
	},
	{
		"set -e; false; echo foo",
		"exit status 1",
	},
	{
		"set -e; shouldnotexist; echo foo",
		"\"shouldnotexist\": executable file not found in $PATH\nexit status 127 #JUSTERR",
	},
	{
		"set -e; set +e; false; echo foo",
		"foo\n",
	},
	{
		"set -e; ! false; echo foo",
		"foo\n",
	},
	{
		"set -e; ! true; echo foo",
		"foo\n",
	},
	{
		"set -e; if false; then echo foo; fi",
		"",
	},
	{
		"set -e; while false; do echo foo; done",
		"",
	},
	{
		"set -e; false || true",
		"",
	},
	{
		"set -e; false && true; true",
		"",
	},
	{
		"false | :",
		"",
	},
	{
		"set -o pipefail; false | :",
		"exit status 1",
	},
	{
		"set -o pipefail; true | false | true | :",
		"exit status 1",
	},
	{
		"set -o pipefail; set -M 2>/dev/null | false",
		"exit status 1",
	},
	{
		"set -f; >a.x; echo *.x;",
		"*.x\n",
	},
	{
		"set -f; set +f; >a.x; echo *.x;",
		"a.x\n",
	},
	{
		"set -a; foo=bar; $ENV_PROG | grep ^foo=",
		"foo=bar\n",
	},
	{
		"set -a; foo=(b a r); $ENV_PROG | grep ^foo=",
		"exit status 1",
	},
	{
		"foo=bar; set -a; $ENV_PROG | grep ^foo=",
		"exit status 1",
	},
	{
		"a=b; echo $a; set -u; echo $a",
		"b\nb\n",
	},
	{
		"echo $a; set -u; echo $a; echo extra",
		"\na: unbound variable\nexit status 1 #JUSTERR",
	},
	{
		"foo=bar; set -u; echo ${foo/bar/}",
		"\n",
	},
	{
		"foo=bar; set -u; echo ${foo#bar}",
		"\n",
	},
	{
		"set -u; echo ${foo/bar/}",
		"foo: unbound variable\nexit status 1 #JUSTERR",
	},
	{
		"set -u; echo ${foo#bar}",
		"foo: unbound variable\nexit status 1 #JUSTERR",
	},
	// TODO: detect this case as unset
	// {
	// 	"set -u; foo=(bar); echo $foo; echo ${foo[3]}",
	// 	"bar\nfoo: unbound variable\nexit status 1 #JUSTERR",
	// },
	{
		"set -u; foo=(''); echo ${foo[0]}",
		"\n",
	},
	{
		"set -u; echo ${#foo}",
		"foo: unbound variable\nexit status 1 #JUSTERR",
	},
	{
		"set -u; echo ${foo+bar}",
		"\n",
	},
	{
		"set -u; echo ${foo:+bar}",
		"\n",
	},
	{
		"set -u; echo ${foo-bar}",
		"bar\n",
	},
	{
		"set -u; echo ${foo:-bar}",
		"bar\n",
	},
	{
		"set -u; echo ${foo=bar}",
		"bar\n",
	},
	{
		"set -u; echo ${foo:=bar}",
		"bar\n",
	},
	{
		"set -u; echo ${foo?bar}",
		"foo: bar\nexit status 1 #JUSTERR",
	},
	{
		"set -u; echo ${foo:?bar}",
		"foo: bar\nexit status 1 #JUSTERR",
	},
	{
		"set -ue; set -ueo pipefail",
		"",
	},
	{"set -n; echo foo", ""},
	{"set -n; [ wrong", ""},
	{"set -n; set +n; echo foo", ""},
	{
		"set -o foobar",
		"set: invalid option: \"foobar\"\nexit status 2 #JUSTERR",
	},
	{"set -o noexec; echo foo", ""},
	{"set +o noexec; echo foo", "foo\n"},
	{"set -e; set -o | grep -E 'errexit|noexec' | wc -l | tr -d ' '", "2\n"},
	{"set -e; set -o | grep -E 'errexit|noexec' | grep 'on$' | wc -l | tr -d ' '", "1\n"},
	{
		"set -a; set +o",
		`set -o allexport
set +o errexit
set +o noexec
set +o noglob
set +o nounset
set +o xtrace
set +o pipefail
 #IGNORE`,
	},

	// unset
	{
		"a=1; echo $a; unset a; echo $a",
		"1\n\n",
	},
	{
		"notinpath() { echo func; }; notinpath; unset -f notinpath; notinpath",
		"func\n\"notinpath\": executable file not found in $PATH\nexit status 127 #JUSTERR",
	},
	{
		"a=1; a() { echo func; }; unset -f a; echo $a",
		"1\n",
	},
	{
		"a=1; a() { echo func; }; unset -v a; a; echo $a",
		"func\n\n",
	},
	{
		"notinpath=1; notinpath() { echo func; }; notinpath; echo $notinpath; unset notinpath; notinpath; echo $notinpath; unset notinpath; notinpath",
		"func\n1\nfunc\n\n\"notinpath\": executable file not found in $PATH\nexit status 127 #JUSTERR",
	},
	{
		"unset PATH; [[ $PATH == '' ]]",
		"",
	},
	{
		"readonly a=1; echo $a; unset a; echo $a",
		"1\na: readonly variable\n1\n #IGNORE bash prints a warning",
	},
	{
		"f() { local a=1; echo $a; unset a; echo $a; }; f",
		"1\n\n",
	},
	{
		`a=b eval 'echo $a; unset a; echo $a'`,
		"b\n\n",
	},
	{
		`$(unset INTERP_GLOBAL); echo $INTERP_GLOBAL; unset INTERP_GLOBAL; echo $INTERP_GLOBAL`,
		"value\n\n",
	},
	{
		`x=orig; f() { local x=local; unset x; x=still_local; }; f; echo $x`,
		"orig\n",
	},
	{
		`x=orig; f() { local x=local; unset x; [[ -v x ]] && echo set || echo unset; }; f`,
		"unset\n",
	},

	// shopt
	{"set -e; shopt -o | grep -E 'errexit|noexec' | wc -l | tr -d ' '", "2\n"},
	{"set -e; shopt -o | grep -E 'errexit|noexec' | grep 'on$' | wc -l | tr -d ' '", "1\n"},
	{"shopt -s -o noexec; echo foo", ""},
	{"shopt -so noexec; echo foo", ""},
	{"shopt -u -o noexec; echo foo", "foo\n"},
	{"shopt -u globstar; shopt globstar | grep 'off$' | wc -l | tr -d ' '", "1\n"},
	{"shopt -s globstar; shopt globstar | grep 'off$' | wc -l | tr -d ' '", "0\n"},

	// IFS
	{`echo -n "$IFS"`, " \t\n"},
	{`a="x:y:z"; IFS=:; echo $a`, "x y z\n"},
	{`a=(x y z); IFS=-; echo ${a[*]}`, "x y z\n"},
	{`a=(x y z); IFS=-; echo ${a[@]}`, "x y z\n"},
	{`a=(x y z); IFS=-; echo "${a[*]}"`, "x-y-z\n"},
	{`a=(x y z); IFS=-; echo "${a[@]}"`, "x y z\n"},
	{`a="  x y z"; IFS=; echo $a`, "  x y z\n"},
	{`a=(x y z); IFS=; echo "${a[*]}"`, "xyz\n"},
	{`a=(x y z); IFS=-; echo "${!a[@]}"`, "0 1 2\n"},
	{`set -- x y z; IFS=-; echo $*`, "x y z\n"},
	{`set -- x y z; IFS=-; echo "$*"`, "x-y-z\n"},
	{`set -- x y z; IFS=; echo $*`, "x y z\n"},
	{`set -- x y z; IFS=; echo "$*"`, "xyz\n"},

	// builtin
	{"builtin", ""},
	{"builtin noexist", "exit status 1 #JUSTERR"},
	{"builtin echo foo", "foo\n"},
	{
		"echo() { printf 'bar\n'; }; echo foo; builtin echo foo",
		"bar\nfoo\n",
	},

	// type
	{"type", ""},
	{"type for", "for is a shell keyword\n"},
	{"type echo", "echo is a shell builtin\n"},
	{"echo() { :; }; type echo | grep 'is a function'", "echo is a function\n"},
	{"type $PATH_PROG | grep -q -E ' is (/|[A-Z]:)'", ""},
	{"type noexist", "type: noexist: not found\nexit status 1 #JUSTERR"},
	{"PATH=/; type $PATH_PROG", "type: " + pathProg + ": not found\nexit status 1 #JUSTERR"},
	{"shopt -s expand_aliases; alias foo='bar baz'\ntype foo", "foo is aliased to `bar baz'\n"},
	{"alias foo='bar baz'\ntype foo", "type: foo: not found\nexit status 1 #JUSTERR"},
	{"type -p $PATH_PROG | grep -q -E '^(/|[A-Z]:)'", ""},
	{"PATH=/; type -p $PATH_PROG", "exit status 1"},
	{"shopt -s expand_aliases; alias foo='bar'; type -t foo", "alias\n"},
	{"type -t case", "keyword\n"},
	{"foo(){ :; }; type -t foo", "function\n"},
	{"type -t type", "builtin\n"},
	{"type -t $PATH_PROG", "file\n"},
	{"type -t inexisting_dfgsdgfds", "exit status 1"},

	// trap
	{"trap 'echo at_exit' EXIT; true", "at_exit\n"},
	{"trap 'echo on_err' ERR; false; echo FAIL", "on_err\nFAIL\n"},
	{"trap 'echo at_exit' EXIT; trap - EXIT; echo OK", "OK\n"},
	{"set -e; trap 'echo A' ERR EXIT; false; echo FAIL", "A\nA\nexit status 1"},
	{"trap 'foobar' UNKNOWN", "trap: UNKNOWN: invalid signal specification\nexit status 2 #JUSTERR"},
	// TODO: our builtin appears to not receive the piped bytes?
	// {"trap 'echo on_err' ERR; trap | grep -q '.*echo on_err.*'", "trap -- \"echo on_err\" ERR\n"},
	{"trap 'false' ERR EXIT; false", "exit status 1"},

	// eval
	{"eval", ""},
	{"eval ''", ""},
	{"eval echo foo", "foo\n"},
	{"eval 'echo foo'", "foo\n"},
	{"eval 'exit 1'", "exit status 1"},
	{"eval '('", "eval: 1:1: reached EOF without matching ( with )\nexit status 1 #JUSTERR"},
	{"set a b; eval 'echo $@'", "a b\n"},
	{"eval 'a=foo'; echo $a", "foo\n"},
	{`a=b eval "echo $a"`, "\n"},
	{`a=b eval 'echo $a'`, "b\n"},
	{`eval 'echo "\$a"'`, "$a\n"},
	{`a=b eval 'x=y eval "echo \$a \$x"'`, "b y\n"},
	{`a=b eval 'a=y eval "echo $a \$a"'`, "b y\n"},
	{"a=b eval '(echo $a)'", "b\n"},

	// source
	{
		"source",
		"1:1: source: need filename\nexit status 2 #JUSTERR",
	},
	{
		"echo 'echo foo' >a; source a; . a",
		"foo\nfoo\n",
	},
	{
		"echo 'echo $@' >a; source a; source a b c; echo $@",
		"\nb c\n\n",
	},
	{
		"echo 'foo=bar' >a; source a; echo $foo",
		"bar\n",
	},

	// source from PATH
	{
		"mkdir test; echo 'echo foo' >test/a; PATH=$PWD/test source a; . test/a",
		"foo\nfoo\n",
	},

	// source with set and shift
	{
		"echo 'set -- d e f' >a; source a; echo $@",
		"d e f\n",
	},
	{
		"echo 'echo $@' >a; set -- b c; source a; echo $@",
		"b c\nb c\n",
	},
	{
		"echo 'echo $@' >a; set -- b c; source a d e; echo $@",
		"d e\nb c\n",
	},
	{
		"echo 'shift; echo $@' >a; set -- b c; source a d e; echo $@",
		"e\nb c\n",
	},
	{
		"echo 'shift' >a; set -- b c; source a; echo $@",
		"c\n",
	},
	{
		"echo 'shift; set -- $@' >a; set -- b c; source a d e; echo $@",
		"e\n",
	},
	{
		"echo 'set -- g f'>b; echo 'set -- d e f; echo $@; source b;' >a; source a; echo $@",
		"d e f\ng f\n",
	},
	{
		"echo 'set -- g f'>b; echo 'echo $@; set -- d e f; source b;' >a; source a b c; echo $@",
		"b c\ng f\n",
	},
	{
		"echo 'shift; echo $@' >b; echo 'shift; echo $@; source b' >a; source a b c d; echo $@",
		"c d\nd\n\n",
	},
	{
		"echo 'set -- b c d' >b; echo 'source b' >a; set -- a; source a; echo $@",
		"b c d\n",
	},
	{
		"echo 'echo $@' >b; echo 'set -- b c d; source b' >a; set -- a; source a; echo $@",
		"b c d\nb c d\n",
	},
	{
		"echo 'shift; echo $@' >b; echo 'shift; echo $@; source b c d' >a; set -- a b; source a; echo $@",
		"b\nd\nb\n",
	},
	{
		"echo 'set -- a b c' >b; echo 'echo $@; source b; echo $@' >a; source a; echo $@",
		"\na b c\na b c\n",
	},

	// indexed arrays
	{
		"a=foo; echo ${a[0]} ${a[@]} ${a[x]}; echo ${a[1]}",
		"foo foo foo\n\n",
	},
	{
		"a=(); echo ${a[0]} ${a[@]} ${a[x]} ${a[1]}",
		"\n",
	},
	{
		"a=(b c); echo $a; echo ${a[0]}; echo ${a[1]}; echo ${a[x]}",
		"b\nb\nc\nb\n",
	},
	{
		"a=(b c); echo ${a[@]}; echo ${a[*]}",
		"b c\nb c\n",
	},
	{
		"a=(1 2 3); echo ${a[2-1]}; echo $((a[1+1]))",
		"2\n3\n",
	},
	{
		"a=(1 2) x=(); a+=b x+=c; echo ${a[@]}; echo ${x[@]}",
		"1b 2\nc\n",
	},
	{
		"a=(1 2) x=(); a+=(b c) x+=(d e); echo ${a[@]}; echo ${x[@]}",
		"1 2 b c\nd e\n",
	},
	{
		"a=bbb; a+=(c d); echo ${a[@]}",
		"bbb c d\n",
	},
	{
		`a=('a  1' 'b  2'); for e in ${a[@]}; do echo "$e"; done`,
		"a\n1\nb\n2\n",
	},
	{
		`a=('a  1' 'b  2'); for e in "${a[*]}"; do echo "$e"; done`,
		"a  1 b  2\n",
	},
	{
		`a=('a  1' 'b  2'); for e in "${a[@]}"; do echo "$e"; done`,
		"a  1\nb  2\n",
	},
	{
		`a=([1]=y [0]=x); echo ${a[0]}`,
		"x\n",
	},
	{
		`a=(y); a[2]=x; echo ${a[2]}`,
		"x\n",
	},
	{
		`a="y"; a[2]=x; echo ${a[2]}`,
		"x\n",
	},
	{
		`declare -a a=(x y); echo ${a[1]}`,
		"y\n",
	},
	{
		`a=b; echo "${a[@]}"`,
		"b\n",
	},
	{
		`a=(b); echo ${a[3]}`,
		"\n",
	},
	{
		`a=(b); echo ${a[-2]}`,
		"negative array index\nexit status 1 #JUSTERR",
	},

	// associative arrays
	{
		`a=foo; echo ${a[""]} ${a["x"]}`,
		"foo foo\n",
	},
	{
		`declare -A a=(); echo ${a[0]} ${a[@]} ${a[1]} ${a["x"]}`,
		"\n",
	},
	{
		`declare -A a=([x]=b [y]=c); echo $a; echo ${a[0]}; echo ${a["x"]}; echo ${a["_"]}`,
		"\n\nb\n\n",
	},
	{
		`declare -A a=([x]=b [y]=c); for e in ${a[@]}; do echo $e; done | sort`,
		"b\nc\n",
	},
	{
		`declare -A a=([y]=b [x]=c); for e in ${a[*]}; do echo $e; done | sort`,
		"b\nc\n",
	},
	{
		`declare -A a=([x]=a); a["y"]=d; a["x"]=c; for e in ${a[@]}; do echo $e; done | sort`,
		"c\nd\n",
	},
	{
		`declare -A a=([x]=a); a[y]=d; a[x]=c; for e in ${a[@]}; do echo $e; done | sort`,
		"c\nd\n",
	},
	{
		// cheating a little; bash just did a=c
		`a=(["x"]=b ["y"]=c); echo ${a["y"]}`,
		"c\n",
	},
	{
		`declare -A a=(['x']=b); echo ${a['x']} ${a[$'x']} ${a[$"x"]}`,
		"b b b\n",
	},
	{
		`a=(['x']=b); echo ${a['y']}`,
		"\n #IGNORE bash requires -A",
	},

	// weird assignments
	{"a=b; a=(c d); echo ${a[@]}", "c d\n"},
	{"a=(b c); a=d; echo ${a[@]}", "d c\n"},
	{"declare -A a=([x]=b [y]=c); a=d; for e in ${a[@]}; do echo $e; done | sort", "b\nc\nd\n"},
	{"i=3; a=b; a[i]=x; echo ${a[@]}", "b x\n"},
	{"i=3; declare a=(b); a[i]=x; echo ${!a[@]}", "0 3\n"},
	{"i=3; declare -A a=(['x']=b); a[i]=x; for e in ${!a[@]}; do echo $e; done | sort", "i\nx\n"},

	// declare
	{"declare -B foo", "declare: invalid option \"-B\"\nexit status 2 #JUSTERR"},
	{"a=b; declare a; echo $a; declare a=; echo $a", "b\n\n"},
	{"a=b; declare a; echo $a", "b\n"},
	{
		"declare a=b c=(1 2); echo $a; echo ${c[@]}",
		"b\n1 2\n",
	},
	{"a=x; declare $a; echo $a $x", "x\n"},
	{"a=x=y; declare $a; echo $a $x", "x=y y\n"},
	{"a='x=(y)'; declare $a; echo $a $x", "x=(y) (y)\n"},
	{"a='x=b y=c'; declare $a; echo $x $y", "b c\n"},
	{"declare =bar", "declare: invalid name \"\"\nexit status 1 #JUSTERR"},
	{"declare $unset=$unset", "declare: invalid name \"\"\nexit status 1 #JUSTERR"},

	// export
	{"declare foo=bar; $ENV_PROG | grep '^foo='", "exit status 1"},
	{"declare -x foo=bar; $ENV_PROG | grep '^foo='", "foo=bar\n"},
	{"export foo=bar; $ENV_PROG | grep '^foo='", "foo=bar\n"},
	{"foo=bar; export foo; $ENV_PROG | grep '^foo='", "foo=bar\n"},
	{"export foo=bar; foo=baz; $ENV_PROG | grep '^foo='", "foo=baz\n"},
	{"export foo=bar; readonly foo=baz; $ENV_PROG | grep '^foo='", "foo=baz\n"},
	{"export foo=(1 2); $ENV_PROG | grep '^foo='", "exit status 1"},
	{"declare -A foo=([a]=b); export foo; $ENV_PROG | grep '^foo='", "exit status 1"},
	{"export foo=(b c); foo=x; $ENV_PROG | grep '^foo='", "exit status 1"},
	{"foo() { bar=foo; export bar; }; foo; $ENV_PROG | grep ^bar=", "bar=foo\n"},
	{"foo() { export bar; }; bar=foo; foo; $ENV_PROG | grep ^bar=", "bar=foo\n"},
	{"foo() { export bar; }; foo; bar=foo; $ENV_PROG | grep ^bar=", "bar=foo\n"},
	{"foo() { export bar=foo; }; foo; readonly bar; $ENV_PROG | grep ^bar=", "bar=foo\n"},

	// local
	{
		"local a=b",
		"local: can only be used in a function\nexit status 1 #JUSTERR",
	},
	{
		"local a=b 2>/dev/null; echo $a",
		"\n",
	},
	{
		"{ local a=b; }",
		"local: can only be used in a function\nexit status 1 #JUSTERR",
	},
	{
		"echo 'local a=b' >a; source a",
		"local: can only be used in a function\nexit status 1 #JUSTERR",
	},
	{
		"echo 'local a=b' >a; f() { source a; }; f; echo $a",
		"\n",
	},
	{
		"f() { local a=b; }; f; echo $a",
		"\n",
	},
	{
		"a=x; f() { local a=b; }; f; echo $a",
		"x\n",
	},
	{
		"a=x; f() { echo $a; local a=b; echo $a; }; f",
		"x\nb\n",
	},
	{
		"f1() { local a=b; }; f2() { f1; echo $a; }; f2",
		"\n",
	},
	{
		"f() { a=1; declare b=2; export c=3; readonly d=4; declare -g e=5; }; f; echo $a $b $c $d $e",
		"1 3 4 5\n",
	},
	{
		`f() { local x; [[ -v x ]] && echo set || echo unset; }; f`,
		"unset\n",
	},
	{
		`f() { local x=; [[ -v x ]] && echo set || echo unset; }; f`,
		"set\n",
	},
	{
		`export x=before; f() { local x; export x=after; $ENV_PROG | grep '^x='; }; f; echo $x`,
		"x=after\nbefore\n",
	},

	// name references
	{"declare -n foo=bar; bar=etc; [[ -R foo ]]", ""},
	{"declare -n foo=bar; bar=etc; [ -R foo ]", ""},
	{"nameref foo=bar; bar=etc; [[ -R foo ]]", " #IGNORE"},
	{"declare foo=bar; bar=etc; [[ -R foo ]]", "exit status 1"},
	{
		"declare -n foo=bar; bar=etc; echo $foo; bar=zzz; echo $foo",
		"etc\nzzz\n",
	},
	{
		"declare -n foo=bar; bar=(x y); echo ${foo[1]}; bar=(a b); echo ${foo[1]}",
		"y\nb\n",
	},
	{
		"declare -n foo=bar; bar=etc; echo $foo; unset bar; echo $foo",
		"etc\n\n",
	},
	{
		"declare -n a1=a2 a2=a3 a3=a4; a4=x; echo $a1 $a3",
		"x x\n",
	},
	{
		"declare -n foo=bar bar=foo; echo $foo",
		"\n #IGNORE",
	},
	{
		"declare -n foo=bar; echo $foo",
		"\n",
	},
	{
		"declare -n foo=bar; echo ${!foo}",
		"bar\n",
	},
	{
		"declare -n foo=bar; bar=etc; echo $foo; echo ${!foo}",
		"etc\nbar\n",
	},
	{
		"declare -n foo=bar; bar=etc; foo=xxx; echo $foo $bar",
		"xxx xxx\n",
	},
	{
		"declare -n foo=bar; foo=xxx; echo $foo $bar",
		"xxx xxx\n",
	},
	// TODO: figure this one out
	//{
	//        "declare -n foo=bar bar=baz; foo=xxx; echo $foo $bar; echo $baz",
	//        "xxx xxx\nxxx\n",
	//},

	// read-only vars
	{"declare -r foo=bar; echo $foo", "bar\n"},
	{"readonly foo=bar; echo $foo", "bar\n"},
	{"readonly foo=bar; export foo; echo $foo", "bar\n"},
	{"readonly foo=bar; readonly bar=foo; export foo bar; echo $bar", "foo\n"},
	{
		"a=b; a=c; echo $a; readonly a; a=d",
		"c\na: readonly variable\nexit status 1 #JUSTERR",
	},
	{
		"declare -r foo=bar; foo=etc",
		"foo: readonly variable\nexit status 1 #JUSTERR",
	},
	{
		"declare -r foo=bar; export foo=",
		"foo: readonly variable\nexit status 1 #JUSTERR",
	},
	{
		"readonly foo=bar; foo=etc",
		"foo: readonly variable\nexit status 1 #JUSTERR",
	},
	{
		"foo() { bar=foo; readonly bar; }; foo; bar=bar",
		"bar: readonly variable\nexit status 1 #JUSTERR",
	},
	{
		"foo() { readonly bar; }; foo; bar=foo",
		"bar: readonly variable\nexit status 1 #JUSTERR",
	},
	{
		"foo() { readonly bar=foo; }; foo; export bar; $ENV_PROG | grep '^bar='",
		"bar=foo\n",
	},

	// multiple var modes at once
	{
		"declare -r -x foo=bar; $ENV_PROG | grep '^foo='",
		"foo=bar\n",
	},
	{
		"declare -r -x foo=bar; foo=x",
		"foo: readonly variable\nexit status 1 #JUSTERR",
	},

	// globbing
	{"echo .", ".\n"},
	{"echo ..", "..\n"},
	{"echo ./.", "./.\n"},
	{
		">a.x >b.x >c.x; echo *.x; rm a.x b.x c.x",
		"a.x b.x c.x\n",
	},
	{
		`>a.x; echo '*.x' "*.x"; rm a.x`,
		"*.x *.x\n",
	},
	{
		`>a.x >b.y; echo *'.'x; rm a.x`,
		"a.x\n",
	},
	{
		`>a.x; echo *'.x' "a."* '*'.x; rm a.x`,
		"a.x a.x *.x\n",
	},
	{
		"echo *.x; echo foo *.y bar",
		"*.x\nfoo *.y bar\n",
	},
	{
		"mkdir a; >a/b.x; echo */*.x | sed 's@\\\\@/@g'; cd a; echo *.x",
		"a/b.x\nb.x\n",
	},
	{
		"mkdir -p a/b/c; echo a/* | sed 's@\\\\@/@g'",
		"a/b\n",
	},
	{
		">.hidden >a; echo *; echo .h*; rm .hidden a",
		"a\n.hidden\n",
	},
	{
		`mkdir d; >d/.hidden >d/a; set -- "$(echo d/*)" "$(echo d/.h*)"; echo ${#1} ${#2}; rm -r d`,
		"3 9\n",
	},
	{
		"mkdir -p a/b/c; echo a/** | sed 's@\\\\@/@g'",
		"a/b\n",
	},
	{
		"shopt -s globstar; mkdir -p a/b/c; echo a/** | sed 's@\\\\@/@g'",
		"a/ a/b a/b/c\n",
	},
	{
		"shopt -s globstar; mkdir -p a/b/c; echo **/c | sed 's@\\\\@/@g'",
		"a/b/c\n",
	},
	{
		"shopt -s nullglob; touch existing-1; echo missing-* existing-*",
		"existing-1\n",
	},
	{
		"cat <<EOF\n{foo,bar}\nEOF",
		"{foo,bar}\n",
	},
	{
		"cat <<EOF\n*.go\nEOF",
		"*.go\n",
	},
	{
		"mkdir -p a/b a/c; echo ./a/* | sed 's@\\\\@/@g'",
		"./a/b ./a/c\n",
	},
	{
		"mkdir -p a/b a/c d; cd d; echo ../a/* | sed 's@\\\\@/@g'",
		"../a/b ../a/c\n",
	},
	{
		"mkdir x-d1 x-d2; >x-f; echo x-*/ | sed 's@\\\\@/@g'",
		"x-d1/ x-d2/\n",
	},
	{
		"mkdir x-d1 x-d2; >x-f; echo ././x-*/// | sed 's@\\\\@/@g'",
		"././x-d1/ ././x-d2/\n",
	},
	{
		"mkdir -p x-d1/a x-d2/b; >x-f; echo x-*/* | sed 's@\\\\@/@g'",
		"x-d1/a x-d2/b\n",
	},
	{
		"mkdir -p foo/bar; ln -s foo sym; echo sy*/; echo sym/b*",
		"sym/\nsym/bar\n",
	},
	{
		">foo; ln -s foo sym; echo sy*; echo sy*/",
		"sym\nsy*/\n",
	},
	{
		"mkdir x-d; >x-f; test -d $PWD/x-*/",
		"",
	},
	{
		"mkdir dir; >dir/x-f; ln -s dir sym; cd sym; test -f $PWD/x-*",
		"",
	},

	// brace expansion; more exhaustive tests in the syntax package
	{"echo a}b", "a}b\n"},
	{"echo {a,b{c,d}", "{a,bc {a,bd\n"},
	{"echo a{b}", "a{b}\n"},
	{"echo a{à,世界}", "aà a世界\n"},
	{"echo a{b,c}d{e,f}g", "abdeg abdfg acdeg acdfg\n"},
	{"echo a{b{x,y},c}d", "abxd abyd acd\n"},
	{"echo a{1..", "a{1..\n"},
	{"echo a{1..2}b{4..5}c", "a1b4c a1b5c a2b4c a2b5c\n"},
	{"echo a{c..f}", "ac ad ae af\n"},
	{"echo a{4..1..1}", "a4 a3 a2 a1\n"},

	// tilde expansion
	{
		"[[ '~/foo' == ~/foo ]] || [[ ~/foo == '~/foo' ]]",
		"exit status 1",
	},
	{
		"case '~/foo' in ~/foo) echo match ;; esac",
		"",
	},
	{
		"a=~/foo; [[ $a == '~/foo' ]]",
		"exit status 1",
	},
	{
		`a=$(echo "~/foo"); [[ $a == '~/foo' ]]`,
		"",
	},

	// /dev/null
	{"echo foo >/dev/null", ""},
	{"cat </dev/null", ""},

	// time - real would be slow and flaky; see TestElapsedString
	{"{ time; } |& wc | tr -s ' '", " 4 6 42\n"},
	{"{ time echo -n; } |& wc | tr -s ' '", " 4 6 42\n"},
	{"{ time -p; } |& wc | tr -s ' '", " 3 6 29\n"},
	{"{ time -p echo -n; } |& wc | tr -s ' '", " 3 6 29\n"},

	// exec
	{"exec", ""},
	{
		"exec builtin echo foo",
		"\"builtin\": executable file not found in $PATH\nexit status 127 #JUSTERR",
	},
	{
		"exec $GOSH_PROG 'echo foo'; echo bar",
		"foo\n",
	},

	// read
	{
		"read </dev/null",
		"exit status 1",
	},
	{
		"read -X",
		"read: invalid option \"-X\"\nexit status 2 #JUSTERR",
	},
	{
		"read -rX",
		"read: invalid option \"-X\"\nexit status 2 #JUSTERR",
	},
	{
		"read 0ab",
		"read: invalid identifier \"0ab\"\nexit status 2 #JUSTERR",
	},
	{
		"read <<< foo; echo $REPLY",
		"foo\n",
	},
	{
		"read <<<'  a  b  c  '; echo \"$REPLY\"",
		"  a  b  c  \n",
	},
	{
		"read <<< 'y\nn\n'; echo $REPLY",
		"y\n",
	},
	{
		"read a_0 <<< foo; echo $a_0",
		"foo\n",
	},
	{
		"read a b <<< 'foo  bar  baz  '; echo \"$a\"; echo \"$b\"",
		"foo\nbar  baz\n",
	},
	{
		"while read a; do echo $a; done <<< 'a\nb\nc'",
		"a\nb\nc\n",
	},
	{
		"while read a b; do echo -e \"$a\n$b\"; done <<< '1 2\n3'",
		"1\n2\n3\n\n",
	},
	{
		`read a <<< '\\'; echo "$a"`,
		"\\\n",
	},
	{
		`read a <<< '\a\b\c'; echo "$a"`,
		"abc\n",
	},
	{
		"read -r a b <<< '1\\\t2'; echo $a; echo $b;",
		"1\\\n2\n",
	},
	{
		"echo line\\\ncontinuation | while read a; do echo $a; done",
		"linecontinuation\n",
	},
	{
		`read -r a <<< '\\'; echo "$a"`,
		"\\\\\n",
	},
	{
		"read -r a <<< '\\a\\b\\c'; echo $a",
		"\\a\\b\\c\n",
	},
	{
		"IFS=: read a b c <<< '1:2:3'; echo $a; echo $b; echo $c",
		"1\n2\n3\n",
	},
	{
		"IFS=: read a b c <<< '1\\:2:3'; echo \"$a\"; echo $b; echo $c",
		"1:2\n3\n\n",
	},
	{
		"read -p",
		"read: -p: option requires an argument\nexit status 2 #JUSTERR",
	},
	{
		"read -X -p",
		"read: invalid option \"-X\"\nexit status 2 #JUSTERR",
	},
	{
		"read -p 'Display me as a prompt. Continue? (y/n) ' choice <<< 'y'; echo $choice",
		"Display me as a prompt. Continue? (y/n) y\n #IGNORE bash requires a terminal",
	},
	{
		"read -r -p 'Prompt and raw flag together: ' a <<< '\\a\\b\\c'; echo $a",
		"Prompt and raw flag together: \\a\\b\\c\n #IGNORE bash requires a terminal",
	},

	// getopts
	{
		"getopts",
		"getopts: usage: getopts optstring name [arg ...]\nexit status 2",
	},
	{
		"getopts a a:b",
		"getopts: invalid identifier: \"a:b\"\nexit status 2 #JUSTERR",
	},
	{
		"getopts abc opt -a; echo $opt; $optarg",
		"a\n",
	},
	{
		"getopts abc opt -z",
		"getopts: illegal option -- \"z\"\n #IGNORE",
	},
	{
		"getopts a: opt -a",
		"getopts: option requires an argument -- \"a\"\n #IGNORE",
	},
	{
		"getopts :abc opt -z; echo $opt; echo $OPTARG",
		"?\nz\n",
	},
	{
		"getopts :a: opt -a; echo $opt; echo $OPTARG",
		":\na\n",
	},
	{
		"getopts abc opt foo -a; echo $opt; echo $OPTIND",
		"?\n1\n",
	},
	{
		"getopts abc opt -a foo; echo $opt; echo $OPTIND",
		"a\n2\n",
	},
	{
		"OPTIND=3; getopts abc opt -a -b -c; echo $opt;",
		"c\n",
	},
	{
		"OPTIND=100; getopts abc opt -a -b -c; echo $opt;",
		"?\n",
	},
	{
		"OPTIND=foo; getopts abc opt -a -b -c; echo $opt;",
		"a\n",
	},
	{
		"while getopts ab:c opt -c -b arg -a foo; do echo $opt $OPTARG $OPTIND; done",
		"c 2\nb arg 4\na 5\n",
	},
	{
		"while getopts abc opt -ba -c foo; do echo $opt $OPTARG $OPTIND; done",
		"b 1\na 2\nc 3\n",
	},
	{
		"a() { while getopts abc: opt; do echo $opt $OPTARG; done }; a -a -b -c arg",
		"a\nb\nc arg\n",
	},
}

var runTestsUnix = []runTest{
	{"[[ -n $PPID && $PPID -gt 0 ]]", ""},
	{
		// no root user on windows
		"[[ ~root == '~root' ]]",
		"exit status 1",
	},

	// windows does not support paths with '*'
	{
		"mkdir -p '*/a.z' 'b/a.z'; cd '*'; set -- *.z; echo $#",
		"1\n",
	},
	{
		"mkdir -p 'a-*/d'; test -d $PWD/a-*/*",
		"",
	},

	// no fifos on windows
	{
		"[ -p a ] && echo x; mkfifo a; [ -p a ] && echo y",
		"y\n",
	},
	{
		"[[ -p a ]] && echo x; mkfifo a; [[ -p a ]] && echo y",
		"y\n",
	},

	{"sh() { :; }; sh -c 'echo foo'", ""},
	{"sh() { :; }; command sh -c 'echo foo'", "foo\n"},

	// chmod is practically useless on Windows
	{
		"[ -x a ] && echo x; >a; chmod 0755 a; [ -x a ] && echo y",
		"y\n",
	},
	{
		"[[ -x a ]] && echo x; >a; chmod 0755 a; [[ -x a ]] && echo y",
		"y\n",
	},
	{
		">a; [ -k a ] && echo x; chmod +t a; [ -k a ] && echo y",
		"y\n",
	},
	{
		">a; [ -u a ] && echo x; chmod u+s a; [ -u a ] && echo y",
		"y\n",
	},
	{
		">a; [ -g a ] && echo x; chmod g+s a; [ -g a ] && echo y",
		"y\n",
	},
	{
		">a; [[ -k a ]] && echo x; chmod +t a; [[ -k a ]] && echo y",
		"y\n",
	},
	{
		">a; [[ -u a ]] && echo x; chmod u+s a; [[ -u a ]] && echo y",
		"y\n",
	},
	{
		">a; [[ -g a ]] && echo x; chmod g+s a; [[ -g a ]] && echo y",
		"y\n",
	},
	{
		`mkdir a; chmod 0100 a; cd a`,
		"",
	},
	// Note that these will succeed if we're root.
	{
		`mkdir a; chmod 0000 a; cd a && test $UID -ne 0`,
		"exit status 1 #JUSTERR",
	},
	{
		`mkdir a; chmod 0222 a; cd a && test $UID -ne 0`,
		"exit status 1 #JUSTERR",
	},
	{
		`mkdir a; chmod 0444 a; cd a && test $UID -ne 0`,
		"exit status 1 #JUSTERR",
	},
	{
		`mkdir a; chmod 0010 a; cd a && test $UID -ne 0`,
		"exit status 1 #JUSTERR",
	},
	{
		`mkdir a; chmod 0001 a; cd a && test $UID -ne 0`,
		"exit status 1 #JUSTERR",
	},
	{
		`unset UID`,
		"UID: readonly variable\n #IGNORE",
	},
	// GID is not set in bash
	{
		`unset GID`,
		"GID: readonly variable\n #IGNORE",
	},
	{
		`[[ -z $GID ]] && echo "GID not set"`,
		"exit status 1 #JUSTERR #IGNORE",
	},

	// Unix-y PATH
	{
		"PATH=; bash -c 'echo foo'",
		"\"bash\": executable file not found in $PATH\nexit status 127 #JUSTERR",
	},
	{
		"cd /; sure/is/missing",
		"stat /sure/is/missing: no such file or directory\nexit status 127 #JUSTERR",
	},
	{
		"echo '#!/bin/sh\necho b' >a; chmod 0755 a; PATH=; a",
		"b\n",
	},
	{
		"mkdir c; cd c; echo '#!/bin/sh\necho b' >a; chmod 0755 a; PATH=; a",
		"b\n",
	},
	{
		"mkdir c; echo '#!/bin/sh\necho b' >c/a; chmod 0755 c/a; c/a",
		"b\n",
	},
	{
		"GOSH_CMD=lookpath $GOSH_PROG",
		"sh found\n",
	},

	// error strings which are too different on Windows
	{
		"echo foo >/shouldnotexist/file",
		"open /shouldnotexist/file: no such file or directory\nexit status 1 #JUSTERR",
	},
	{
		"set -e; echo foo >/shouldnotexist/file; echo foo",
		"open /shouldnotexist/file: no such file or directory\nexit status 1 #JUSTERR",
	},

	// process substitution; named pipes (fifos) are a TODO for windows
	{
		"sed 's/o/e/g' <(echo foo bar)",
		"fee bar\n",
	},
	{
		"cat <(echo foo) <(echo bar) <(echo baz)",
		"foo\nbar\nbaz\n",
	},
	{
		"cat <(cat <(echo nested))",
		"nested\n",
	},
	{
		"echo foo bar > >(sed 's/o/e/g')",
		"fee bar\n",
	},
	{
		"echo foo bar | tee >(sed 's/o/e/g') >/dev/null",
		"fee bar\n",
	},
	{
		"echo nested > >(cat > >(cat))",
		"nested\n",
	},
	// echo trace
	{
		`set -x; animals=("dog", "cat", "otter"); echo "hello ${animals[*]}"`,
		`+ animals=("dog", "cat", "otter")
+ echo 'hello dog, cat, otter'
hello dog, cat, otter
`,
	},
	{
		`set -x; s="always print a decimal point for %e, %E, %f, %F, %g and %G; do not remove trailing zeros for %g and %G"; echo "$s"`,
		`+ s='always print a decimal point for %e, %E, %f, %F, %g and %G; do not remove trailing zeros for %g and %G'
+ echo 'always print a decimal point for %e, %E, %f, %F, %g and %G; do not remove trailing zeros for %g and %G'
always print a decimal point for %e, %E, %f, %F, %g and %G; do not remove trailing zeros for %g and %G
`,
	},
	{
		`set -x
x=without; echo "$x"
x="double quote"; echo "$x"
x='single quote'; echo "$x"`,
		`+ x=without
+ echo without
without
+ x='double quote'
+ echo 'double quote'
double quote
+ x='single quote'
+ echo 'single quote'
single quote
`,
	},
	// for trace
	{
		`set -x
animals=(dog, cat, otter)
for i in ${animals[@]}
do
   echo "hello ${i}"
done
`,
		`+ animals=(dog, cat, otter)
+ for i in ${animals[@]}
+ echo 'hello dog,'
hello dog,
+ for i in ${animals[@]}
+ echo 'hello cat,'
hello cat,
+ for i in ${animals[@]}
+ echo 'hello otter'
hello otter
`,
	},
	{
		`set -x
loop() {
    for i do
        echo "something with $i"
    done
}
loop 1 2 3`,
		`+ loop 1 2 3
+ for i in "$@"
+ echo 'something with 1'
something with 1
+ for i in "$@"
+ echo 'something with 2'
something with 2
+ for i in "$@"
+ echo 'something with 3'
something with 3
`,
	},
	{
		`set -x; animals=(dog, cat, otter); for i in ${animals[@]}; do echo "hello ${i}"; done`,
		`+ animals=(dog, cat, otter)
+ for i in ${animals[@]}
+ echo 'hello dog,'
hello dog,
+ for i in ${animals[@]}
+ echo 'hello cat,'
hello cat,
+ for i in ${animals[@]}
+ echo 'hello otter'
hello otter
`,
	},
	{
		`set -x; a=x"y"$z b=(foo bar $none '')`,
		"+ a=xy\n+ b=(foo bar $none '')\n",
	},
	{
		`set -x; for i in a b; do echo $i; done`,
		`+ for i in a b
+ echo a
a
+ for i in a b
+ echo b
b
`,
	},
	{
		`set -x; for i in $none_a $none_b; do echo $i; done`,
		``,
	},
	// case trace
	{
		`set -x; pet=dog; case $pet in 'dog') echo "barks";; *) echo "unknown";; esac`,
		`+ pet=dog
+ case $pet in
+ echo barks
barks
`,
	},
	{
		`set -x
pet="dog"
case $pet in
  dog)
    echo "barks"
    ;;
  *)
    echo "unknown"
    ;;
esac`,
		`+ pet=dog
+ case $pet in
+ echo barks
barks
`,
	},
	// arithmetic
	{
		`set -x
a=$(( 4 + 5 )); echo $a
a=$((3+5)); echo $a`,
		`+ a=9
+ echo 9
9
+ a=8
+ echo 8
8
`,
	},
	{
		`set -x;
let a=5+4; echo $a
let "a = 5 + 4"; echo $a
let a++; echo $a`,
		`+ let a=5+4
+ echo 9
9
+ let 'a = 5 + 4'
+ echo 9
9
+ let a++
+ echo 10
10
`,
	},
	// functions
	{
		`set -x; function with_function () { echo 'hello, world'; }; with_function`,
		`+ with_function
+ echo 'hello, world'
hello, world
`,
	},
	{
		`set -x; without_function () { echo 'hello, world'; }; without_function`,
		`+ without_function
+ echo 'hello, world'
hello, world
`,
	},
}

var runTestsWindows = []runTest{
	{"[[ -n $PPID || $PPID -gt 0 ]]", ""}, // os.Getppid can be 0 on windows
	{"cmd() { :; }; cmd /c 'echo foo'", ""},
	{"cmd() { :; }; command cmd /c 'echo foo'", "foo\r\n"},
	{
		"GOSH_CMD=lookpath $GOSH_PROG",
		"cmd found\n",
	},
}

// These tests are specific to 64-bit architectures, and that's fine. We don't
// need to add explicit versions for 32-bit.
var runTests64bit = []runTest{
	{"printf %i,%u -3 -3", "-3,18446744073709551613"},
	{"printf %o -3", "1777777777777777777775"},
	{"printf %x -3", "fffffffffffffffd"},
}

func init() {
	if runtime.GOOS == "windows" {
		runTests = append(runTests, runTestsWindows...)
	} else { // Unix-y
		runTests = append(runTests, runTestsUnix...)
	}
	if bits.UintSize == 64 {
		runTests = append(runTests, runTests64bit...)
	}
}

// ln -s: wine doesn't implement symlinks; see https://bugs.winehq.org/show_bug.cgi?id=44948
var skipOnWindows = regexp.MustCompile(`ln -s`)

// process substitutions seemflaky on mac; see https://github.com/mvdan/sh/issues/576
var skipOnMac = regexp.MustCompile(`>\(|<\(`)

func skipIfUnsupported(tb testing.TB, src string) {
	switch {
	case runtime.GOOS == "windows" && skipOnWindows.MatchString(src):
		tb.Skipf("skipping non-portable test on windows")
	case runtime.GOOS == "darwin" && skipOnMac.MatchString(src):
		tb.Skipf("skipping non-portable test on mac")
	}
}

func TestRunnerRun(t *testing.T) {
	t.Parallel()

	p := syntax.NewParser()
	for _, c := range runTests {
		c := c
		t.Run("", func(t *testing.T) {
			skipIfUnsupported(t, c.in)

			// Parse first, as we reuse a single parser.
			file := parse(t, p, c.in)

			t.Parallel()

			tdir := t.TempDir()
			var cb concBuffer
			r, err := New(Dir(tdir), StdIO(nil, &cb, &cb),
				// TODO: why does this make some tests hang?
				// Env(expand.ListEnviron(append(os.Environ(),
				// 	"FOO_NULL_BAR=foo\x00bar")...)),
				OpenHandler(testOpenHandler),
				ExecHandler(testExecHandler),
			)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), runnerRunTimeout)
			defer cancel()
			if err := r.Run(ctx, file); err != nil {
				cb.WriteString(err.Error())
			}
			want := c.want
			if i := strings.Index(want, " #"); i >= 0 {
				want = want[:i]
			}
			if got := cb.String(); got != want {
				if len(got) > 200 {
					got = "…" + got[len(got)-200:]
				}
				t.Fatalf("wrong output in %q:\nwant: %q\ngot:  %q",
					c.in, want, got)
			}
		})
	}
}

func readLines(hc HandlerContext) ([][]byte, error) {
	bs, err := io.ReadAll(hc.Stdin)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS == "windows" {
		bs = bytes.ReplaceAll(bs, []byte("\r\n"), []byte("\n"))
	}
	bs = bytes.TrimSuffix(bs, []byte("\n"))
	return bytes.Split(bs, []byte("\n")), nil
}

var testBuiltinsMap = map[string]func(HandlerContext, []string) error{
	"cat": func(hc HandlerContext, args []string) error {
		if len(args) == 0 {
			if hc.Stdin == nil || hc.Stdout == nil {
				return nil
			}
			_, err := io.Copy(hc.Stdout, hc.Stdin)
			return err
		}
		for _, arg := range args {
			path := absPath(hc.Dir, arg)
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(hc.Stdout, f)
			f.Close()
			if err != nil {
				return err
			}
		}
		return nil
	},
	"wc": func(hc HandlerContext, args []string) error {
		bs, err := io.ReadAll(hc.Stdin)
		if err != nil {
			return err
		}
		if len(args) == 0 {
			fmt.Fprintf(hc.Stdout, "%7d", bytes.Count(bs, []byte("\n")))
			fmt.Fprintf(hc.Stdout, "%8d", len(bytes.Fields(bs)))
			fmt.Fprintf(hc.Stdout, "%8d\n", len(bs))
		} else if args[0] == "-c" {
			fmt.Fprintln(hc.Stdout, len(bs))
		} else if args[0] == "-l" {
			fmt.Fprintln(hc.Stdout, bytes.Count(bs, []byte("\n")))
		}
		return nil
	},
	"tr": func(hc HandlerContext, args []string) error {
		if len(args) != 2 || len(args[1]) != 1 {
			return fmt.Errorf("usage: tr [-s -d] [character]")
		}
		squeeze := args[0] == "-s"
		char := args[1][0]
		bs, err := io.ReadAll(hc.Stdin)
		if err != nil {
			return err
		}
		for {
			i := bytes.IndexByte(bs, char)
			if i < 0 {
				hc.Stdout.Write(bs) // remaining
				break
			}
			hc.Stdout.Write(bs[:i]) // up to char
			bs = bs[i+1:]

			bs = bytes.TrimLeft(bs, string(char)) // remove repeats
			if squeeze {
				hc.Stdout.Write([]byte{char})
			}
		}
		return nil
	},
	"sort": func(hc HandlerContext, args []string) error {
		lines, err := readLines(hc)
		if err != nil {
			return err
		}
		sort.Slice(lines, func(i, j int) bool {
			return bytes.Compare(lines[i], lines[j]) < 0
		})
		for _, line := range lines {
			fmt.Fprintf(hc.Stdout, "%s\n", line)
		}
		return nil
	},
	"grep": func(hc HandlerContext, args []string) error {
		var rx *regexp.Regexp
		quiet := false
		for _, arg := range args {
			if arg == "-q" {
				quiet = true
			} else if arg == "-E" {
			} else if rx == nil {
				rx = regexp.MustCompile(arg)
			} else {
				return fmt.Errorf("unexpected arg: %q", arg)
			}
		}
		lines, err := readLines(hc)
		if err != nil {
			return err
		}
		any := false
		for _, line := range lines {
			if rx.Match(line) {
				if quiet {
					return nil
				}
				any = true
				fmt.Fprintf(hc.Stdout, "%s\n", line)
			}
		}
		if !any {
			return NewExitStatus(1)
		}
		return nil
	},
	"sed": func(hc HandlerContext, args []string) error {
		f := hc.Stdin
		switch len(args) {
		case 1:
		case 2:
			var err error
			f, err = os.Open(absPath(hc.Dir, args[1]))
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("usage: sed pattern [file]")
		}
		expr := args[0]
		if expr == "" || expr[0] != 's' {
			return fmt.Errorf("unimplemented")
		}
		sep := expr[1]
		expr = expr[2:]
		from := expr[:strings.IndexByte(expr, sep)]
		expr = expr[len(from)+1:]
		to := expr[:strings.IndexByte(expr, sep)]
		bs, err := io.ReadAll(f)
		if err != nil {
			return err
		}
		rx := regexp.MustCompile(from)
		bs = rx.ReplaceAllLiteral(bs, []byte(to))
		_, err = hc.Stdout.Write(bs)
		return err
	},
	"mkdir": func(hc HandlerContext, args []string) error {
		for _, arg := range args {
			if arg == "-p" {
				continue
			}
			path := absPath(hc.Dir, arg)
			if err := os.MkdirAll(path, 0o777); err != nil {
				return err
			}
		}
		return nil
	},
	"rm": func(hc HandlerContext, args []string) error {
		for _, arg := range args {
			if arg == "-r" {
				continue
			}
			path := absPath(hc.Dir, arg)
			if err := os.RemoveAll(path); err != nil {
				return err
			}
		}
		return nil
	},
	"ln": func(hc HandlerContext, args []string) error {
		symbolic := args[0] == "-s"
		if symbolic {
			args = args[1:]
		}
		oldname := absPath(hc.Dir, args[0])
		newname := absPath(hc.Dir, args[1])
		if symbolic {
			return os.Symlink(oldname, newname)
		}
		return os.Link(oldname, newname)
	},
	"touch": func(hc HandlerContext, args []string) error {
		newTime := time.Now()
		if args[0] == "-d" {
			if !strings.HasPrefix(args[1], "@") {
				return fmt.Errorf("unimplemented")
			}
			sec, err := strconv.ParseInt(args[1][1:], 10, 64)
			if err != nil {
				return err
			}
			newTime = time.Unix(sec, 0)
			args = args[2:]
		}
		for _, arg := range args {
			path := absPath(hc.Dir, arg)
			// create the file if it does not exist
			f, err := os.OpenFile(path, os.O_CREATE, 0o666)
			if err != nil {
				return err
			}
			f.Close()
			// change the modification and access time
			if err := os.Chtimes(path, newTime, newTime); err != nil {
				return err
			}
		}
		return nil
	},
	"sleep": func(hc HandlerContext, args []string) error {
		// Note that, unlike GNU sleep, we don't assume a default unit
		// of seconds.
		for _, arg := range args {
			d, err := time.ParseDuration(arg)
			if err != nil {
				return err
			}
			time.Sleep(d)
		}
		return nil
	},
}

func testExecHandler(ctx context.Context, args []string) error {
	if fn := testBuiltinsMap[args[0]]; fn != nil {
		return fn(HandlerCtx(ctx), args[1:])
	}
	return DefaultExecHandler(2*time.Second)(ctx, args)
}

func testOpenHandler(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
	if runtime.GOOS == "windows" && path == "/dev/null" {
		path = "NUL"
	}

	return DefaultOpenHandler()(ctx, path, flag, perm)
}

func TestRunnerRunConfirm(t *testing.T) {
	if testing.Short() {
		t.Skip("calling bash is slow")
	}
	if !hasBash50 {
		t.Skip("bash 5.0 required to run")
	}
	t.Parallel()

	if runtime.GOOS == "windows" {
		// For example, it seems to treat environment variables as
		// case-sensitive, which isn't how Windows works.
		t.Skip("bash on Windows emulates Unix-y behavior")
	}
	for _, c := range runTests {
		c := c
		t.Run("", func(t *testing.T) {
			if strings.Contains(c.want, " #IGNORE") {
				return
			}
			skipIfUnsupported(t, c.in)
			t.Parallel()
			tdir := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), runnerRunTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash")
			cmd.Dir = tdir
			cmd.Stdin = strings.NewReader(c.in)
			out, err := cmd.CombinedOutput()
			if strings.Contains(c.want, " #JUSTERR") {
				// bash sometimes exits with status code 0 and
				// stderr "bash: ..." for an error
				fauxErr := bytes.HasPrefix(out, []byte("bash:"))
				if err == nil && !fauxErr {
					t.Fatalf("wanted bash to error in %q", c.in)
				}
				return
			}
			got := string(out)
			if err != nil {
				got += err.Error()
			}
			if got != c.want {
				t.Fatalf("wrong bash output in %q:\nwant: %q\ngot:  %q",
					c.in, c.want, got)
			}
		})
	}
}

func TestRunnerOpts(t *testing.T) {
	t.Parallel()

	withPath := func(strs ...string) func(*Runner) error {
		prefix := []string{
			"PATH=" + os.Getenv("PATH"),
			"ENV_PROG=" + os.Getenv("ENV_PROG"),
		}
		return Env(expand.ListEnviron(append(prefix, strs...)...))
	}
	opts := func(list ...RunnerOption) []RunnerOption {
		return list
	}
	cases := []struct {
		opts     []RunnerOption
		in, want string
	}{
		{
			nil,
			"$ENV_PROG | grep '^INTERP_GLOBAL='",
			"INTERP_GLOBAL=value\n",
		},
		{
			opts(withPath()),
			"$ENV_PROG | grep '^INTERP_GLOBAL='",
			"exit status 1",
		},
		{
			opts(withPath("INTERP_GLOBAL=bar")),
			"$ENV_PROG | grep '^INTERP_GLOBAL='",
			"INTERP_GLOBAL=bar\n",
		},
		{
			opts(withPath("a=b")),
			"echo $a",
			"b\n",
		},
		{
			opts(withPath("A=b")),
			"$ENV_PROG | grep '^A='; echo $A",
			"A=b\nb\n",
		},
		{
			opts(withPath("A=b", "A=c")),
			"$ENV_PROG | grep '^A='; echo $A",
			"A=c\nc\n",
		},
		{
			opts(withPath("HOME=")),
			"echo $HOME",
			"\n",
		},
		{
			opts(withPath("PWD=foo")),
			"[[ $PWD == foo ]]",
			"exit status 1",
		},
		{
			opts(Params("foo")),
			"echo $@",
			"foo\n",
		},
		{
			opts(Params("-u", "--", "foo")),
			"echo $@; echo $unset",
			"foo\nunset: unbound variable\nexit status 1",
		},
		{
			opts(Params("-u", "--", "foo")),
			"echo $@; echo ${unset:-default}",
			"foo\ndefault\n",
		},
		{
			opts(Params("foo")),
			"set >/dev/null; echo $@",
			"foo\n",
		},
		{
			opts(Params("foo")),
			"set -e; echo $@",
			"foo\n",
		},
		{
			opts(Params("foo")),
			"set --; echo $@",
			"\n",
		},
		{
			opts(Params("foo")),
			"set bar; echo $@",
			"bar\n",
		},
	}
	p := syntax.NewParser()
	for _, c := range cases {
		t.Run("", func(t *testing.T) {
			skipIfUnsupported(t, c.in)
			file := parse(t, p, c.in)
			var cb concBuffer
			r, err := New(append(c.opts,
				StdIO(nil, &cb, &cb),
				OpenHandler(testOpenHandler),
				ExecHandler(testExecHandler),
			)...)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), runnerRunTimeout)
			defer cancel()
			if err := r.Run(ctx, file); err != nil {
				cb.WriteString(err.Error())
			}
			if got := cb.String(); got != c.want {
				t.Fatalf("wrong output in %q:\nwant: %q\ngot:  %q",
					c.in, c.want, got)
			}
		})
	}
}

func TestRunnerContext(t *testing.T) {
	t.Parallel()

	cases := []string{
		"",
		"while true; do true; done",
		"until false; do true; done",
		"sleep 1000",
		"while true; do true; done & wait",
		"sleep 1000 & wait",
		"(while true; do true; done)",
		"$(while true; do true; done)",
		"while true; do true; done | while true; do true; done",
	}
	p := syntax.NewParser()
	for _, in := range cases {
		t.Run("", func(t *testing.T) {
			file := parse(t, p, in)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			r, _ := New()
			errChan := make(chan error)
			go func() {
				errChan <- r.Run(ctx, file)
			}()

			timeout := 500 * time.Millisecond
			select {
			case err := <-errChan:
				if err != nil && err != ctx.Err() {
					t.Fatal("Runner did not use ctx.Err()")
				}
			case <-time.After(timeout):
				t.Fatalf("program was not killed in %s", timeout)
			}
		})
	}
}

func TestRunnerAltNodes(t *testing.T) {
	t.Parallel()

	in := "echo foo"
	file := parse(t, nil, in)
	want := "foo\n"
	nodes := []syntax.Node{
		file,
		file.Stmts[0],
		file.Stmts[0].Cmd,
	}
	for _, node := range nodes {
		var cb concBuffer
		r, _ := New(StdIO(nil, &cb, &cb))
		ctx, cancel := context.WithTimeout(context.Background(), runnerRunTimeout)
		defer cancel()
		if err := r.Run(ctx, node); err != nil {
			cb.WriteString(err.Error())
		}
		if got := cb.String(); got != want {
			t.Fatalf("wrong output in %q:\nwant: %q\ngot:  %q",
				in, want, got)
		}
	}
}

func TestElapsedString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in    time.Duration
		posix bool
		want  string
	}{
		{time.Nanosecond, false, "0m0.000s"},
		{time.Millisecond, false, "0m0.001s"},
		{time.Millisecond, true, "0.00"},
		{2500 * time.Millisecond, false, "0m2.500s"},
		{2500 * time.Millisecond, true, "2.50"},
		{
			10*time.Minute + 10*time.Second,
			false,
			"10m10.000s",
		},
		{
			10*time.Minute + 10*time.Second,
			true,
			"610.00",
		},
		{31 * time.Second, false, "0m31.000s"},
		{102 * time.Second, false, "1m42.000s"},
	}
	for _, tc := range tests {
		t.Run(tc.in.String(), func(t *testing.T) {
			got := elapsedString(tc.in, tc.posix)
			if got != tc.want {
				t.Fatalf("wanted %q, got %q", tc.want, got)
			}
		})
	}
}

func TestRunnerDir(t *testing.T) {
	t.Parallel()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("Missing", func(t *testing.T) {
		_, err := New(Dir("missing"))
		if err == nil {
			t.Fatal("expected New to error when Dir is missing")
		}
	})
	t.Run("NotDir", func(t *testing.T) {
		_, err := New(Dir("interp_test.go"))
		if err == nil {
			t.Fatal("expected New to error when Dir is not a dir")
		}
	})
	t.Run("NotDirAbs", func(t *testing.T) {
		_, err := New(Dir(filepath.Join(wd, "interp_test.go")))
		if err == nil {
			t.Fatal("expected New to error when Dir is not a dir")
		}
	})
	t.Run("Relative", func(t *testing.T) {
		// On Windows, it's impossible to make a relative path from one
		// drive to another. Use the parent directory, as that's for
		// sure in the same drive as the current directory.
		rel := ".." + string(filepath.Separator)
		r, err := New(Dir(rel))
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsAbs(r.Dir) {
			t.Errorf("Runner.Dir is not absolute")
		}
	})
	// Ensure that we treat symlinks and short paths properly, especially
	// with Dir and globbing.
	t.Run("SymlinkOrShortPath", func(t *testing.T) {
		tdir := t.TempDir()

		realDir := filepath.Join(tdir, "real-long-dir-name")
		realFile := filepath.Join(realDir, "realfile")

		if err := os.Mkdir(realDir, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(realFile, []byte(""), 0o666); err != nil {
			t.Fatal(err)
		}

		var altDir string
		if runtime.GOOS == "windows" {
			short, err := shortPathName(realDir)
			if err != nil {
				t.Fatal(err)
			}
			altDir = short
			// We replace tdir later, and it might have been shortened.
			tdir = filepath.Dir(altDir)
		} else {
			altDir = filepath.Join(tdir, "symlink")
			if err := os.Symlink(realDir, altDir); err != nil {
				t.Fatal(err)
			}
		}

		var b bytes.Buffer
		r, err := New(Dir(altDir), StdIO(nil, &b, &b))
		if err != nil {
			t.Fatal(err)
		}
		file := parse(t, nil, "echo $PWD $PWD/*")
		ctx, cancel := context.WithTimeout(context.Background(), runnerRunTimeout)
		defer cancel()
		if err := r.Run(ctx, file); err != nil {
			t.Fatal(err)
		}
		got := b.String()
		got = strings.ReplaceAll(got, tdir, "")
		got = strings.TrimSpace(got)
		want := `/symlink /symlink/realfile`
		if runtime.GOOS == "windows" {
			want = `\\REAL.{4} \\REAL.{4}\\realfile`
		}
		if !regexp.MustCompile(want).MatchString(got) {
			t.Fatalf("\nwant regexp: %q\ngot: %q", want, got)
		}
	})
}

func TestRunnerIncremental(t *testing.T) {
	t.Parallel()

	file := parse(t, nil, "echo foo; false; echo bar; exit 0; echo baz")
	want := "foo\nbar\n"
	var b bytes.Buffer
	r, _ := New(StdIO(nil, &b, &b))
	ctx, cancel := context.WithTimeout(context.Background(), runnerRunTimeout)
	defer cancel()
	for _, stmt := range file.Stmts {
		err := r.Run(ctx, stmt)
		if _, ok := IsExitStatus(err); !ok && err != nil {
			// Keep track of unexpected errors.
			b.WriteString(err.Error())
		}
		if r.Exited() {
			break
		}
	}
	if got := b.String(); got != want {
		t.Fatalf("\nwant: %q\ngot:  %q", want, got)
	}
}

func TestRunnerResetFields(t *testing.T) {
	t.Parallel()

	tdir := t.TempDir()
	logPath := filepath.Join(tdir, "log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	r, _ := New(
		Params("-f", "--", "first", tdir, logPath),
		Dir(tdir),
		OpenHandler(testOpenHandler),
		ExecHandler(testExecHandler),
	)
	// Check that using option funcs and Runner fields directly is still
	// kept by Reset.
	StdIO(nil, logFile, os.Stderr)(r)
	r.Env = expand.ListEnviron(append(os.Environ(), "GLOBAL=foo")...)

	file := parse(t, nil, `
# Params set 3 arguments
[[ $# -eq 3 ]] || exit 10
[[ $1 == "first" ]] || exit 11

# Params set the -f option (noglob)
[[ -o noglob ]] || exit 12

# $PWD was set via Dir, and should be equal to $2
[[ "$PWD" == "$2" ]] || exit 13

# stdout should go into the log file, which is at $3
echo line1
echo line2
[[ "$(wc -l <$3)" == "2" ]] || exit 14

# $GLOBAL was set directly via the Env field
[[ "$GLOBAL" == "foo" ]] || exit 15

# Change all of the above within the script. Reset should undo this.
set +f -- newargs
cd
exec >/dev/null 2>/dev/null
GLOBAL=
export GLOBAL=
`)
	ctx, cancel := context.WithTimeout(context.Background(), runnerRunTimeout)
	defer cancel()
	for i := 0; i < 3; i++ {
		if err := r.Run(ctx, file); err != nil {
			t.Fatalf("run number %d: %v", i, err)
		}
		r.Reset()
		// empty the log file too
		logFile.Truncate(0)
		logFile.Seek(0, io.SeekStart)
	}
}

func TestRunnerManyResets(t *testing.T) {
	t.Parallel()
	r, _ := New()
	for i := 0; i < 5; i++ {
		r.Reset()
	}
}

func TestRunnerFilename(t *testing.T) {
	t.Parallel()

	want := "f.sh\n"
	file, _ := syntax.NewParser().Parse(strings.NewReader("echo $0"), "f.sh")
	var b bytes.Buffer
	r, _ := New(StdIO(nil, &b, &b))
	ctx, cancel := context.WithTimeout(context.Background(), runnerRunTimeout)
	defer cancel()
	if err := r.Run(ctx, file); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != want {
		t.Fatalf("\nwant: %q\ngot:  %q", want, got)
	}
}

func TestRunnerEnvNoModify(t *testing.T) {
	t.Parallel()

	env := expand.ListEnviron("one=1", "two=2")
	file := parse(t, nil, `echo -n "$one $two; "; one=x; unset two`)

	var b bytes.Buffer
	r, _ := New(Env(env), StdIO(nil, &b, &b))
	ctx, cancel := context.WithTimeout(context.Background(), runnerRunTimeout)
	defer cancel()
	for i := 0; i < 3; i++ {
		r.Reset()
		err := r.Run(ctx, file)
		if err != nil {
			t.Fatal(err)
		}
	}

	want := "1 2; 1 2; 1 2; "
	if got := b.String(); got != want {
		t.Fatalf("\nwant: %q\ngot:  %q", want, got)
	}
}

func TestMalformedPathOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Skipping windows test on non-windows GOOS")
	}
	tdir := t.TempDir()
	t.Parallel()

	path := filepath.Join(tdir, "test.cmd")
	script := []byte("@echo foo")
	if err := os.WriteFile(path, script, 0o777); err != nil {
		t.Fatal(err)
	}

	// set PATH to c:\tmp\dir instead of C:\tmp\dir
	volume := filepath.VolumeName(tdir)
	pathList := strings.ToLower(volume) + tdir[len(volume):]

	file := parse(t, nil, "test.cmd")
	var cb concBuffer
	r, _ := New(Env(expand.ListEnviron("PATH="+pathList)), StdIO(nil, &cb, &cb))
	ctx, cancel := context.WithTimeout(context.Background(), runnerRunTimeout)
	defer cancel()
	if err := r.Run(ctx, file); err != nil {
		t.Fatal(err)
	}
	want := "foo\r\n"
	if got := cb.String(); got != want {
		t.Fatalf("wrong output:\nwant: %q\ngot:  %q", want, got)
	}
}

func TestReadShouldNotPanicWithNilStdin(t *testing.T) {
	t.Parallel()

	r, err := New()
	if err != nil {
		t.Fatal(err)
	}

	f := parse(t, nil, "read foobar")
	ctx, cancel := context.WithTimeout(context.Background(), runnerRunTimeout)
	defer cancel()
	if err := r.Run(ctx, f); err == nil {
		t.Fatal("it should have retuned an error")
	}
}

func TestRunnerVars(t *testing.T) {
	t.Parallel()

	r, err := New()
	if err != nil {
		t.Fatal(err)
	}

	f := parse(t, nil, "FOO=updated; BAR=new")
	ctx, cancel := context.WithTimeout(context.Background(), runnerRunTimeout)
	defer cancel()
	if err := r.Run(ctx, f); err != nil {
		t.Fatal(err)
	}

	if want, got := "updated", r.Vars["FOO"].String(); got != want {
		t.Fatalf("wrong output:\nwant: %q\ngot:  %q", want, got)
	}
}

func TestRunnerSubshell(t *testing.T) {
	t.Parallel()

	r1, err := New()
	if err != nil {
		t.Fatal(err)
	}

	r2 := r1.Subshell()
	f1 := parse(t, nil, "PARENT=foo")
	f2 := parse(t, nil, "CHILD=bar")

	ctx, cancel := context.WithTimeout(context.Background(), runnerRunTimeout)
	defer cancel()
	if err := r1.Run(ctx, f1); err != nil {
		t.Fatal(err)
	}
	if err := r2.Run(ctx, f2); err != nil {
		t.Fatal(err)
	}

	if want, got := "foo", r1.Vars["PARENT"].String(); got != want {
		t.Fatalf("wrong output:\nwant: %q\ngot:  %q", want, got)
	}
	if want, got := "bar", r2.Vars["CHILD"].String(); got != want {
		t.Fatalf("wrong output:\nwant: %q\ngot:  %q", want, got)
	}

	r3 := r2.Subshell()
	f3 := parse(t, nil, "CHILD=modified")
	if err := r3.Run(ctx, f3); err != nil {
		t.Fatal(err)
	}
	if want, got := "bar", r2.Vars["CHILD"].String(); got != want {
		t.Fatalf("wrong output:\nwant: %q\ngot:  %q", want, got)
	}
	if want, got := "modified", r3.Vars["CHILD"].String(); got != want {
		t.Fatalf("wrong output:\nwant: %q\ngot:  %q", want, got)
	}
}
