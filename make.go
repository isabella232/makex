package makex

import (
	"fmt"
	"io"
	"os"
	"os/exec"

	"code.google.com/p/rog-go/parallel"
)

// TargetsNeedingBuild returns an ordered list of target sets
func (c *Config) NewMaker(mf *Makefile, goals ...string) *Maker {
	m := &Maker{
		mf:     mf,
		goals:  goals,
		Config: c,
	}
	m.buildDAG()
	return m
}

type Maker struct {
	mf     *Makefile
	goals  []string
	dag    map[string][]string
	topo   [][]string
	cycles map[string][]string

	// RuleOutput specifies the writers to receive the stdout and stderr output
	// from executing a rule's recipes. If RuleOutput is nil, os.Stdout and
	// os.Stderr are used, respectively.
	RuleOutput func(r Rule) (out io.Writer, err io.Writer)

	*Config
}

func (m *Maker) buildDAG() {
	// topological sort taken from
	// http://rosettacode.org/wiki/Topological_sort#Go.

	if m.dag == nil || m.cycles == nil {
		m.dag = make(map[string][]string)
		m.cycles = make(map[string][]string)
	}

	seen := make(map[string]struct{})
	queue := append([]string{}, m.goals...)
	for {
		if len(queue) == 0 {
			break
		}
		origLen := len(queue)
		for _, target := range queue {
			if _, seen := seen[target]; seen {
				continue
			}
			seen[target] = struct{}{}

			rule := m.mf.Rule(target)
			if rule == nil {
				continue
			}
			m.dag[target] = append([]string{}, rule.Prereqs()...)
			for _, dep := range rule.Prereqs() {
				// make a node for the prereq target even if it isn't defined
				queue = append(queue, dep)
				m.dag[dep] = m.dag[dep]
			}
		}
		queue = queue[origLen:]
	}

	// topological sort on the DAG
	for len(m.dag) > 0 {

		// collect targets with no dependencies
		var zero []string
		for target, prereqs := range m.dag {
			if len(prereqs) == 0 {
				zero = append(zero, target)
				delete(m.dag, target)
			}
		}

		// cycle detection
		if len(zero) == 0 {
			// collect un-orderable dependencies
			cycle := make(map[string]bool)
			for _, prereqs := range m.dag {
				for _, dep := range prereqs {
					cycle[dep] = true
				}
			}

			// mark targets with un-orderable dependencies
			for target, prereqs := range m.dag {
				if cycle[target] {
					m.cycles[target] = prereqs
				}
			}
			return
		}

		// output a set that can be processed concurrently
		m.topo = append(m.topo, zero)

		// remove edges (dependencies) from dg
		for _, remove := range zero {
			for target, prereqs := range m.dag {
				for i, dep := range prereqs {
					if dep == remove {
						copy(prereqs[i:], prereqs[i+1:])
						m.dag[target] = prereqs[:len(prereqs)-1]
						break
					}
				}
			}
		}
	}
}

func (m *Maker) TargetSets() [][]string {
	return m.topo
}

func (m *Maker) TargetSetsNeedingBuild() ([][]string, error) {
	for _, goal := range m.goals {
		if rule := m.mf.Rule(goal); rule == nil {
			return nil, errNoRuleToMakeTarget(goal)
		}
		if deps, isCycle := m.cycles[goal]; isCycle {
			return nil, errCircularDependency(goal, deps)
		}
	}

	targetSets := make([][]string, 0)
	for _, targetSet := range m.topo {
		var targetsNeedingBuild []string
		for _, target := range targetSet {
			exists, err := m.pathExists(target)
			if err != nil {
				return nil, err
			}
			if !exists {
				rule := m.mf.Rule(target)
				if rule == nil {
					return nil, errNoRuleToMakeTarget(target)
				}
				targetsNeedingBuild = append(targetsNeedingBuild, target)
			}
		}
		if len(targetsNeedingBuild) > 0 {
			targetSets = append(targetSets, targetsNeedingBuild)
		}
	}
	return targetSets, nil
}

// DryRun prints information about what targets *would* be built if Run() was
// called.
func (m *Maker) DryRun(w io.Writer) error {
	targetSets, err := m.TargetSetsNeedingBuild()
	if err != nil {
		return err
	}
	if len(targetSets) == 0 {
		fmt.Fprintln(w, "No target sets need building.")
	}
	for i, targetSet := range targetSets {
		if i != 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "========= TARGET SET %d (%d targets)\n", i, len(targetSet))
		for _, target := range targetSet {
			fmt.Fprintln(w, " - ", target)
		}
	}
	return nil
}

// ruleOutput determines the io.Writers to receive the stderr and stdout output
// of a rule's recipe commands.
func (m *Maker) ruleOutput(r Rule) (stdout io.Writer, stderr io.Writer) {
	if m.RuleOutput != nil {
		return m.RuleOutput(r)
	}
	return os.Stdout, os.Stderr
}

func (m *Maker) Run() error {
	targetSets, err := m.TargetSetsNeedingBuild()
	if err != nil {
		return err
	}

	for _, targetSet := range targetSets {
		par := parallel.NewRun(m.ParallelJobs)
		for _, target := range targetSet {
			rule := m.mf.Rule(target)
			stdout, stderr := m.ruleOutput(rule)
			par.Do(func() error {
				for _, recipe := range rule.Recipes() {
					recipe = ExpandAutoVars(rule, recipe)
					if m.Verbose {
						m.Log.Printf("[%s] %s", rule.Target(), recipe)
					}
					cmd := exec.Command("sh", "-c", recipe)
					cmd.Stdout, cmd.Stderr = stdout, stderr

					err := cmd.Run()
					if err != nil {
						// remove files if failed
						if exists, _ := m.pathExists(rule.Target()); exists {
							err2 := m.fs().Remove(rule.Target())
							if err2 != nil {
								m.Log.Printf("[%s] failed removing target after error: %s", rule.Target(), err)
							}
						}

						m.Log.Printf(`command failed:
============================================================
FAIL: %s
============================================================
`, recipe)
						return fmt.Errorf("[%s] command %q failed: %s", rule.Target(), recipe, err)
					}
				}
				return nil
			})
		}
		err := par.Wait()
		if err != nil {
			return Errors(err.(parallel.Errors))
		}
	}

	return nil
}

func errNoRuleToMakeTarget(target string) error {
	return fmt.Errorf("no rule to make target %q", target)
}

func errCircularDependency(target string, deps []string) error {
	return fmt.Errorf("circular dependency for target %q: %v", target, deps)
}
