package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	. "launchpad.net/gocheck"
	_ "launchpad.net/juju-core/environs/dummy"
	"launchpad.net/juju-core/testing"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	stdtesting "testing"
)

func TestPackage(t *stdtesting.T) {
	testing.MgoTestPackage(t)
}

type MainSuite struct{}

var _ = Suite(&MainSuite{})

var flagRunMain = flag.Bool("run-main", false, "Run the application's main function for recursive testing")

// Reentrancy point for testing (something as close as possible to) the juju
// tool itself.
func TestRunMain(t *stdtesting.T) {
	if *flagRunMain {
		Main(flag.Args())
	}
}

func badrun(c *C, exit int, cmd ...string) string {
	args := append([]string{"-test.run", "TestRunMain", "-run-main", "--", "juju"}, cmd...)
	ps := exec.Command(os.Args[0], args...)
	output, err := ps.CombinedOutput()
	if exit != 0 {
		c.Assert(err, ErrorMatches, fmt.Sprintf("exit status %d", exit))
	}
	return string(output)
}

var runMainTests = []struct {
	summary string
	args    []string
	code    int
	out     string
}{
	{
		summary: "missing command",
		code:    2,
		out:     "error: no command specified\n",
	}, {
		summary: "unknown command",
		args:    []string{"discombobulate"},
		code:    2,
		out:     "error: unrecognized command: juju discombobulate\n",
	}, {
		summary: "unknown option before command",
		args:    []string{"--cheese", "bootstrap"},
		code:    2,
		out:     "error: flag provided but not defined: --cheese\n",
	}, {
		summary: "unknown option after command",
		args:    []string{"bootstrap", "--cheese"},
		code:    2,
		out:     "error: flag provided but not defined: --cheese\n",
	}, {
		summary: "known option, but specified before command",
		args:    []string{"--environment", "blah", "bootstrap"},
		code:    2,
		out:     "error: flag provided but not defined: --environment\n",
	},
}

func (s *MainSuite) TestRunMain(c *C) {
	for i, t := range runMainTests {
		c.Logf("test %d: %s", i, t.summary)
		out := badrun(c, t.code, t.args...)
		c.Assert(out, Equals, t.out)
	}
}

var brokenConfig = `
environments:
    one:
        type: dummy
        state-server: false
        authorized-keys: i-am-a-key
        broken: %s
`

// breakJuju forces the dummy environment to return an error
// when environMethod is called.
func breakJuju(c *C, environMethod string) (msg string) {
	yaml := fmt.Sprintf(brokenConfig, environMethod)
	err := ioutil.WriteFile(homePath(".juju", "environments.yaml"), []byte(yaml), 0666)
	c.Assert(err, IsNil)

	return fmt.Sprintf("dummy.%s is broken", environMethod)
}

func (s *MainSuite) TestActualRunJujuArgsBeforeCommand(c *C) {
	defer makeFakeHome(c, "one").restore()

	// Check global args work when specified before command
	msg := breakJuju(c, "Bootstrap")
	logpath := filepath.Join(c.MkDir(), "log")
	out := badrun(c, 1, "--log-file", logpath, "--verbose", "--debug", "bootstrap")
	c.Assert(out, Equals, "error: "+msg+"\n")
	content, err := ioutil.ReadFile(logpath)
	c.Assert(err, IsNil)
	fullmsg := fmt.Sprintf(`(.|\n)*JUJU juju bootstrap command failed: %s\n`, msg)
	c.Assert(string(content), Matches, fullmsg)
}

func (s *MainSuite) TestActualRunJujuArgsAfterCommand(c *C) {
	defer makeFakeHome(c, "one").restore()

	// Check global args work when specified after command
	msg := breakJuju(c, "Bootstrap")
	logpath := filepath.Join(c.MkDir(), "log")
	out := badrun(c, 1, "bootstrap", "--log-file", logpath, "--verbose", "--debug")
	c.Assert(out, Equals, "error: "+msg+"\n")
	content, err := ioutil.ReadFile(logpath)
	c.Assert(err, IsNil)
	fullmsg := fmt.Sprintf(`(.|\n)*JUJU juju bootstrap command failed: %s\n`, msg)
	c.Assert(string(content), Matches, fullmsg)
}

var commandNames = []string{
	"add-relation",
	"add-unit",
	"bootstrap",
	"deploy",
	"destroy-environment",
	"destroy-relation",
	"destroy-service",
	"destroy-unit",
	"expose",
	"get",
	"remove-relation",
	"remove-unit",
	"resolved",
	"scp",
	"set",
	"ssh",
	"status",
	"unexpose",
	"upgrade-juju",
}

func (s *MainSuite) TestHelp(c *C) {
	// Check that we have correctly registered all the commands
	// by checking the help output.

	out := badrun(c, 0, "-help")
	lines := strings.Split(out, "\n")
	c.Assert(lines[0], Matches, `usage: juju .*`)
	for ; len(lines) > 0; lines = lines[1:] {
		if lines[0] == "commands:" {
			break
		}
	}
	c.Assert(lines, Not(HasLen), 0)

	var names []string
	for lines = lines[1:]; len(lines) > 0; lines = lines[1:] {
		f := strings.Fields(lines[0])
		if len(f) == 0 {
			continue
		}
		c.Assert(f, Not(HasLen), 0)
		names = append(names, f[0])
	}
	sort.Strings(names)
	c.Assert(names, DeepEquals, commandNames)
}

type fakeHome string

func makeFakeHome(c *C, certNames ...string) fakeHome {
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", c.MkDir())

	err := os.Mkdir(homePath(".juju"), 0777)
	c.Assert(err, IsNil)
	for _, name := range certNames {
		err := ioutil.WriteFile(homePath(".juju", name+"-cert.pem"), []byte(testing.CACert), 0666)
		c.Assert(err, IsNil)

		err = ioutil.WriteFile(homePath(".juju", name+"-private-key.pem"), []byte(testing.CAKey), 0666)
		c.Assert(err, IsNil)
	}

	err = os.Mkdir(homePath(".ssh"), 0777)
	c.Assert(err, IsNil)
	err = ioutil.WriteFile(homePath(".ssh", "id_rsa.pub"), []byte("auth key\n"), 0666)
	c.Assert(err, IsNil)

	return fakeHome(oldHome)
}

func homePath(names ...string) string {
	all := append([]string{os.Getenv("HOME")}, names...)
	return filepath.Join(all...)
}

func (h fakeHome) restore() {
	os.Setenv("HOME", string(h))
}
