package vsolver

import (
	"container/heap"
	"fmt"
	"math/rand"
	"strconv"

	"github.com/Sirupsen/logrus"
)

var (
	// With a random revision and no name, collisions are unlikely
	nilpa = ProjectAtom{
		Version: Revision(strconv.FormatInt(rand.Int63(), 36)),
	}
)

func NewSolver(sm SourceManager, l *logrus.Logger) Solver {
	if l == nil {
		l = logrus.New()
	}

	return &solver{
		sm:     sm,
		l:      l,
		latest: make(map[ProjectName]struct{}),
		rlm:    make(map[ProjectName]LockedProject),
	}
}

// solver is a specialized backtracking SAT solver with satisfiability
// conditions hardcoded to the needs of the Go package management problem space.
type solver struct {
	l        *logrus.Logger
	sm       SourceManager
	latest   map[ProjectName]struct{}
	sel      *selection
	unsel    *unselected
	versions []*versionQueue
	rp       ProjectInfo
	rlm      map[ProjectName]LockedProject
	attempts int
}

// Solve takes a ProjectInfo describing the root project, and a list of
// ProjectNames which should be upgraded, and attempts to find a complete
// solution that satisfies all constraints.
func (s *solver) Solve(root ProjectInfo, toUpgrade []ProjectName) Result {
	// local overrides would need to be handled first.
	// TODO local overrides! heh
	s.rp = root

	if root.Lock != nil {
		for _, lp := range root.Lock.Projects() {
			s.rlm[lp.Name] = lp
		}
	}

	for _, v := range toUpgrade {
		s.latest[v] = struct{}{}
	}

	// Initialize queues
	s.sel = &selection{
		deps: make(map[ProjectName][]Dependency),
	}
	s.unsel = &unselected{
		sl:  make([]ProjectName, 0),
		cmp: s.unselectedComparator,
	}

	// Prime the queues with the root project
	s.selectVersion(s.rp.pa)

	// Prep is done; actually run the solver
	var r Result
	r.Projects, r.SolveFailure = s.solve()
	return r
}

func (s *solver) solve() ([]ProjectAtom, error) {
	for {
		ref, has := s.nextUnselected()

		if !has {
			// no more packages to select - we're done. bail out
			break
		}

		if s.l.Level >= logrus.DebugLevel {
			s.l.WithFields(logrus.Fields{
				"attempts": s.attempts,
				"name":     ref,
				"selcount": len(s.sel.projects),
			}).Debug("Beginning step in solve loop")
		}

		queue, err := s.createVersionQueue(ref)

		if err != nil {
			// Err means a failure somewhere down the line; try backtracking.
			if s.backtrack() {
				// backtracking succeeded, move to the next unselected ref
				continue
			}
			return nil, err
		}

		if queue.current() == nil {
			panic("canary - queue is empty, but flow indicates success")
		}

		if s.l.Level >= logrus.InfoLevel {
			s.l.WithFields(logrus.Fields{
				"name":    queue.ref,
				"version": queue.current(),
			}).Info("Accepted project atom")
		}

		s.selectVersion(ProjectAtom{
			Name:    queue.ref,
			Version: queue.current(),
		})
		s.versions = append(s.versions, queue)
	}

	// Getting this far means we successfully found a solution
	var projs []ProjectAtom
	for _, p := range s.sel.projects {
		projs = append(projs, p)
	}
	return projs, nil
}

func (s *solver) createVersionQueue(ref ProjectName) (*versionQueue, error) {
	// If on the root package, there's no queue to make
	if ref == s.rp.Name() {
		return newVersionQueue(ref, nilpa, s.sm)
	}

	exists, err := s.sm.RepoExists(ref)
	if err != nil {
		return nil, err
	}
	if !exists {
		exists, err = s.sm.VendorCodeExists(ref)
		if err != nil {
			return nil, err
		}
		if exists {
			// Project exists only in vendor (and in some manifest somewhere)
			// TODO mark this for special handling, somehow?
			if s.l.Level >= logrus.WarnLevel {
				s.l.WithFields(logrus.Fields{
					"name": ref,
				}).Warn("Code found in vendor for project, but no history was found upstream or in cache")
			}
		} else {
			if s.l.Level >= logrus.WarnLevel {
				s.l.WithFields(logrus.Fields{
					"name": ref,
				}).Warn("Upstream project does not exist")
			}
			return nil, newSolveError(fmt.Sprintf("Project '%s' could not be located.", ref), cannotResolve)
		}
	}

	lockv := s.getLockVersionIfValid(ref)

	q, err := newVersionQueue(ref, lockv, s.sm)
	if err != nil {
		// TODO this particular err case needs to be improved to be ONLY for cases
		// where there's absolutely nothing findable about a given project name
		if s.l.Level >= logrus.WarnLevel {
			s.l.WithFields(logrus.Fields{
				"name": ref,
				"err":  err,
			}).Warn("Failed to create a version queue")
		}
		return nil, err
	}

	if s.l.Level >= logrus.DebugLevel {
		if lockv == nilpa {
			s.l.WithFields(logrus.Fields{
				"name":  ref,
				"queue": q,
			}).Debug("Created versionQueue, but no data in lock for project")
		} else {
			s.l.WithFields(logrus.Fields{
				"name":  ref,
				"queue": q,
			}).Debug("Created versionQueue using version found in lock")
		}
	}

	return q, s.findValidVersion(q)
}

// findValidVersion walks through a versionQueue until it finds a version that
// satisfies the constraints held in the current state of the solver.
func (s *solver) findValidVersion(q *versionQueue) error {
	if nil == q.current() {
		// TODO this case shouldn't be reachable, but panic here as a canary
		panic("version queue is empty, should not happen")
	}

	faillen := len(q.fails)

	if s.l.Level >= logrus.DebugLevel {
		s.l.WithFields(logrus.Fields{
			"name":      q.ref,
			"hasLock":   q.hasLock,
			"allLoaded": q.allLoaded,
		}).Debug("Beginning search through versionQueue for a valid version")
	}
	for {
		cur := q.current()
		err := s.satisfiable(ProjectAtom{
			Name:    q.ref,
			Version: cur,
		})
		if err == nil {
			// we have a good version, can return safely
			if s.l.Level >= logrus.DebugLevel {
				s.l.WithFields(logrus.Fields{
					"name":    q.ref,
					"version": cur,
				}).Debug("Found acceptable version, returning out")
			}
			return nil
		}

		if q.advance(err) != nil {
			// Error on advance, have to bail out
			if s.l.Level >= logrus.WarnLevel {
				s.l.WithFields(logrus.Fields{
					"name": q.ref,
					"err":  err,
				}).Warn("Advancing version queue returned unexpected error, marking project as failed")
			}
			break
		}
		if q.isExhausted() {
			// Queue is empty, bail with error
			if s.l.Level >= logrus.InfoLevel {
				s.l.WithField("name", q.ref).Info("Version queue was completely exhausted, marking project as failed")
			}
			break
		}
	}

	s.fail(s.sel.getDependenciesOn(q.ref)[0].Depender.Name)

	// Return a compound error of all the new errors encountered during this
	// attempt to find a new, valid version
	return &noVersionError{
		pn:    q.ref,
		fails: q.fails[faillen:],
	}
}

func (s *solver) getLockVersionIfValid(ref ProjectName) ProjectAtom {
	// If the project is specifically marked for changes, then don't look for a
	// locked version.
	if _, has := s.latest[ref]; has {
		exist, _ := s.sm.RepoExists(ref)
		// For projects without an upstream or cache repository, we still have
		// to try to use what they have in the lock, because that's the only
		// version we'll be able to actually get for them.
		//
		// TODO to make this work well, we need to differentiate between
		// implicit and explicit selection of packages to upgrade (with an 'all'
		// vs itemized approach). Then, if explicit, we have to error out
		// completely...somewhere. But if implicit, it's ok to ignore, albeit
		// with a warning
		if !exist {
			return nilpa
		}
	}

	lp, exists := s.rlm[ref]
	if !exists {
		if s.l.Level >= logrus.DebugLevel {
			s.l.WithField("name", ref).Debug("Project not present in lock")
		}
		return nilpa
	}

	constraint := s.sel.getConstraint(ref)
	if !constraint.Matches(lp.Version) {
		if s.l.Level >= logrus.InfoLevel {
			s.l.WithFields(logrus.Fields{
				"name":    ref,
				"version": lp.Version,
			}).Info("Project found in lock, but version not allowed by current constraints")
		}
		return nilpa
	}

	if s.l.Level >= logrus.InfoLevel {
		s.l.WithFields(logrus.Fields{
			"name":    ref,
			"version": lp.Version,
		}).Info("Project found in lock")
	}

	return ProjectAtom{
		Name:    lp.Name,
		Version: lp.Version,
	}
}

// satisfiable is the main checking method - it determines if introducing a new
// project atom would result in a graph where all requirements are still
// satisfied.
func (s *solver) satisfiable(pi ProjectAtom) error {
	if emptyProjectAtom == pi {
		// TODO we should protect against this case elsewhere, but for now panic
		// to canary when it's a problem
		panic("canary - checking version of empty ProjectAtom")
	}

	if s.l.Level >= logrus.DebugLevel {
		s.l.WithFields(logrus.Fields{
			"name":    pi.Name,
			"version": pi.Version,
		}).Debug("Checking satisfiability of project atom against current constraints")
	}

	constraint := s.sel.getConstraint(pi.Name)
	if !constraint.Matches(pi.Version) {
		// TODO collect constraint failure reason

		if s.l.Level >= logrus.InfoLevel {
			s.l.WithFields(logrus.Fields{
				"name":          pi.Name,
				"version":       pi.Version,
				"curconstraint": constraint.String(),
			}).Info("Current constraints do not allow version")
		}

		deps := s.sel.getDependenciesOn(pi.Name)
		var failparent []Dependency
		for _, dep := range deps {
			if !dep.Dep.Constraint.Matches(pi.Version) {
				if s.l.Level >= logrus.DebugLevel {
					s.l.WithFields(logrus.Fields{
						"name":       pi.Name,
						"othername":  dep.Depender.Name,
						"constraint": dep.Dep.Constraint.String(),
					}).Debug("Marking other, selected project with conflicting constraint as failed")
				}
				s.fail(dep.Depender.Name)
				failparent = append(failparent, dep)
			}
		}

		return &versionNotAllowedFailure{
			goal:       pi,
			failparent: failparent,
			c:          constraint,
		}
	}

	deps, err := s.getDependenciesOf(pi)
	if err != nil {
		// An err here would be from the package fetcher; pass it straight back
		return err
	}

	for _, dep := range deps {
		// TODO dart skips "magic" deps here; do we need that?

		siblings := s.sel.getDependenciesOn(dep.Name)

		constraint = s.sel.getConstraint(dep.Name)
		// Ensure the constraint expressed by the dep has at least some possible
		// intersection with the intersection of existing constraints.
		if !constraint.MatchesAny(dep.Constraint) {
			if s.l.Level >= logrus.DebugLevel {
				s.l.WithFields(logrus.Fields{
					"name":          pi.Name,
					"version":       pi.Version,
					"depname":       dep.Name,
					"curconstraint": constraint.String(),
					"newconstraint": dep.Constraint.String(),
				}).Debug("Project atom cannot be added; its constraints are disjoint with existing constraints")
			}

			// No admissible versions - visit all siblings and identify the disagreement(s)
			var failsib []Dependency
			var nofailsib []Dependency
			for _, sibling := range siblings {
				if !sibling.Dep.Constraint.MatchesAny(dep.Constraint) {
					if s.l.Level >= logrus.DebugLevel {
						s.l.WithFields(logrus.Fields{
							"name":          pi.Name,
							"version":       pi.Version,
							"depname":       sibling.Depender.Name,
							"sibconstraint": sibling.Dep.Constraint.String(),
							"newconstraint": dep.Constraint.String(),
						}).Debug("Marking other, selected project as failed because its constraint is disjoint with our testee")
					}
					s.fail(sibling.Depender.Name)
					failsib = append(failsib, sibling)
				} else {
					nofailsib = append(nofailsib, sibling)
				}
			}

			return &disjointConstraintFailure{
				goal:      Dependency{Depender: pi, Dep: dep},
				failsib:   failsib,
				nofailsib: nofailsib,
				c:         constraint,
			}
		}

		selected, exists := s.sel.selected(dep.Name)
		if exists && !dep.Constraint.Matches(selected.Version) {
			if s.l.Level >= logrus.DebugLevel {
				s.l.WithFields(logrus.Fields{
					"name":          pi.Name,
					"version":       pi.Version,
					"depname":       dep.Name,
					"curversion":    selected.Version,
					"newconstraint": dep.Constraint.String(),
				}).Debug("Project atom cannot be added; a constraint it introduces does not allow a currently selected version")
			}
			s.fail(dep.Name)

			return &constraintNotAllowedFailure{
				goal: Dependency{Depender: pi, Dep: dep},
				v:    selected.Version,
			}
		}

		// TODO add check that fails if adding this atom would create a loop
	}

	if s.l.Level >= logrus.DebugLevel {
		s.l.WithFields(logrus.Fields{
			"name":    pi.Name,
			"version": pi.Version,
		}).Debug("Project atom passed satisfiability test against current state")
	}

	return nil
}

// getDependenciesOf returns the dependencies of the given ProjectAtom, mediated
// through any overrides dictated by the root project.
//
// If it's the root project, also includes dev dependencies, etc.
func (s *solver) getDependenciesOf(pi ProjectAtom) ([]ProjectDep, error) {
	info, err := s.sm.GetProjectInfo(pi)
	if err != nil {
		// TODO revisit this once a decision is made about better-formed errors;
		// question is, do we expect the fetcher to pass back simple errors, or
		// well-typed solver errors?
		return nil, err
	}

	deps := info.GetDependencies()
	if s.rp.Name() == pi.Name {
		// Root package has more things to pull in
		deps = append(deps, info.GetDevDependencies()...)

		// TODO add overrides here...if we impl the concept (which we should)
	}

	// TODO we have to validate well-formedness of a project's manifest
	// somewhere. this may be a good spot. alternatively, the fetcher may
	// validate well-formedness, whereas here we validate availability of the
	// named deps here. (the latter is sorta what pub does here)

	return deps, nil
}

// backtrack works backwards from the current failed solution to find the next
// solution to try.
func (s *solver) backtrack() bool {
	if len(s.versions) == 0 {
		// nothing to backtrack to
		return false
	}

	if s.l.Level >= logrus.DebugLevel {
		s.l.WithFields(logrus.Fields{
			"selcount":   len(s.sel.projects),
			"queuecount": len(s.versions),
			"attempts":   s.attempts,
		}).Debug("Beginning backtracking")
	}

	for {
		for {
			if s.l.Level >= logrus.DebugLevel {
				s.l.WithField("queuecount", len(s.versions)).Debug("Top of search loop for failed queues")
			}

			if len(s.versions) == 0 {
				// no more versions, nowhere further to backtrack
				return false
			}
			if s.versions[len(s.versions)-1].failed {
				break
			}

			if s.l.Level >= logrus.InfoLevel {
				s.l.WithFields(logrus.Fields{
					"name":      s.versions[len(s.versions)-1].ref,
					"wasfailed": false,
				}).Info("Backtracking popped off project")
			}
			// pub asserts here that the last in s.sel's ids is == q.current
			s.versions, s.versions[len(s.versions)-1] = s.versions[:len(s.versions)-1], nil
			s.unselectLast()
		}

		// Grab the last versionQueue off the list of queues
		q := s.versions[len(s.versions)-1]

		if s.l.Level >= logrus.DebugLevel {
			s.l.WithFields(logrus.Fields{
				"name":    q.ref,
				"failver": q.current(),
			}).Debug("Trying failed queue with next version")
		}

		// another assert that the last in s.sel's ids is == q.current
		s.unselectLast()

		// Advance the queue past the current version, which we know is bad
		// TODO is it feasible to make available the failure reason here?
		if q.advance(nil) == nil && !q.isExhausted() {
			// Search for another acceptable version of this failed dep in its queue
			if s.findValidVersion(q) == nil {
				if s.l.Level >= logrus.InfoLevel {
					s.l.WithFields(logrus.Fields{
						"name":    q.ref,
						"version": q.current(),
					}).Info("Backtracking found valid version, attempting next solution")
				}

				// Found one! Put it back on the selected queue and stop
				// backtracking
				s.selectVersion(ProjectAtom{
					Name:    q.ref,
					Version: q.current(),
				})
				break
			}
		}

		if s.l.Level >= logrus.DebugLevel {
			s.l.WithFields(logrus.Fields{
				"name": q.ref,
			}).Debug("Failed to find a valid version in queue, continuing backtrack")
		}

		// No solution found; continue backtracking after popping the queue
		// we just inspected off the list
		if s.l.Level >= logrus.InfoLevel {
			s.l.WithFields(logrus.Fields{
				"name":      s.versions[len(s.versions)-1].ref,
				"wasfailed": true,
			}).Info("Backtracking popped off project")
		}
		// GC-friendly pop pointer elem in slice
		s.versions, s.versions[len(s.versions)-1] = s.versions[:len(s.versions)-1], nil
	}

	// Backtracking was successful if loop ended before running out of versions
	if len(s.versions) == 0 {
		return false
	}
	s.attempts++
	return true
}

func (s *solver) nextUnselected() (ProjectName, bool) {
	if len(s.unsel.sl) > 0 {
		return s.unsel.sl[0], true
	}

	return "", false
}

func (s *solver) unselectedComparator(i, j int) bool {
	iname, jname := s.unsel.sl[i], s.unsel.sl[j]

	if iname == jname {
		return false
	}

	rname := s.rp.Name()
	// *always* put root project first
	if iname == rname {
		return true
	}
	if jname == rname {
		return false
	}

	_, ilock := s.rlm[iname]
	_, jlock := s.rlm[jname]

	switch {
	case ilock && !jlock:
		return true
	case !ilock && jlock:
		return false
	case ilock && jlock:
		return iname < jname
	}

	// Now, sort by number of available versions. This will trigger network
	// activity, but at this point we know that the project we're looking at
	// isn't locked by the root. And, because being locked by root is the only
	// way avoid that call when making a version queue, we know we're gonna have
	// to pay that cost anyway.
	//
	// TODO ...at least, 'til we allow 'preferred' versions via non-root locks

	// Ignore err here - if there is actually an issue, it'll be picked up very
	// soon somewhere else saner in the solving algorithm
	ivl, _ := s.sm.ListVersions(iname)
	jvl, _ := s.sm.ListVersions(jname)
	iv, jv := len(ivl), len(jvl)

	// Packages with fewer versions to pick from are less likely to benefit from
	// backtracking, so deal with them earlier in order to minimize the amount
	// of superfluous backtracking through them we do.
	switch {
	case iv == 0 && jv != 0:
		return true
	case iv != 0 && jv == 0:
		return false
	case iv != jv:
		return iv < jv
	}

	// Finally, if all else fails, fall back to comparing by name
	return iname < jname
}

func (s *solver) fail(name ProjectName) {
	// skip if the root project
	if s.rp.Name() == name {
		s.l.Debug("Not marking the root project as failed")
		return
	}

	for _, vq := range s.versions {
		if vq.ref == name {
			vq.failed = true
			// just look for the first (oldest) one; the backtracker will
			// necessarily traverse through and pop off any earlier ones
			return
		}
	}
}

func (s *solver) selectVersion(pa ProjectAtom) {
	s.unsel.remove(pa.Name)
	s.sel.projects = append(s.sel.projects, pa)

	deps, err := s.getDependenciesOf(pa)
	if err != nil {
		// if we're choosing a package that has errors getting its deps, there's
		// a bigger problem
		// TODO try to create a test that hits this
		panic("shouldn't be possible")
	}

	for _, dep := range deps {
		siblingsAndSelf := append(s.sel.getDependenciesOn(dep.Name), Dependency{Depender: pa, Dep: dep})
		s.sel.deps[dep.Name] = siblingsAndSelf

		// add project to unselected queue if this is the first dep on it -
		// otherwise it's already in there, or been selected
		if len(siblingsAndSelf) == 1 {
			heap.Push(s.unsel, dep.Name)
		}
	}
}

func (s *solver) unselectLast() {
	var pa ProjectAtom
	pa, s.sel.projects = s.sel.projects[len(s.sel.projects)-1], s.sel.projects[:len(s.sel.projects)-1]
	heap.Push(s.unsel, pa.Name)

	deps, err := s.getDependenciesOf(pa)
	if err != nil {
		// if we're choosing a package that has errors getting its deps, there's
		// a bigger problem
		// TODO try to create a test that hits this
		panic("shouldn't be possible")
	}

	for _, dep := range deps {
		siblings := s.sel.getDependenciesOn(dep.Name)
		siblings = siblings[:len(siblings)-1]
		s.sel.deps[dep.Name] = siblings

		// if no siblings, remove from unselected queue
		if len(siblings) == 0 {
			if s.l.Level >= logrus.DebugLevel {
				s.l.WithFields(logrus.Fields{
					"name":  dep.Name,
					"pname": pa.Name,
					"pver":  pa.Version,
				}).Debug("Removing project from unselected queue; last parent atom was unselected")
			}
			s.unsel.remove(dep.Name)
		}
	}
}
