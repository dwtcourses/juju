package state

import (
	"errors"
	"fmt"
	"labix.org/v2/mgo"
	"labix.org/v2/mgo/bson"
	"labix.org/v2/mgo/txn"
	"launchpad.net/juju-core/charm"
	"launchpad.net/juju-core/log"
	"launchpad.net/juju-core/trivial"
	"strconv"
)

// Service represents the state of a service.
type Service struct {
	st  *State
	doc serviceDoc
}

// serviceDoc represents the internal state of a service in MongoDB.
type serviceDoc struct {
	Name          string `bson:"_id"`
	CharmURL      *charm.URL
	ForceCharm    bool
	Life          Life
	UnitSeq       int
	UnitCount     int
	RelationCount int
	Exposed       bool
	TxnRevno      int64 `bson:"txn-revno"`
}

func newService(st *State, doc *serviceDoc) *Service {
	return &Service{st: st, doc: *doc}
}

// Name returns the service name.
func (s *Service) Name() string {
	return s.doc.Name
}

// globalKey returns the global database key for the service.
func (s *Service) globalKey() string {
	return "s#" + s.doc.Name
}

// Life returns whether the service is Alive, Dying or Dead.
func (s *Service) Life() Life {
	return s.doc.Life
}

// EnsureDying sets the service lifecycle, and those of all its relations,
// to Dying if the service is Alive. It does nothing otherwise.
func (s *Service) EnsureDying() error {
	// To kill the relations in the same transaction as the service, we need
	// to collect a consistent relation state on which to apply it; and if the
	// transaction is aborted, we need to re-collect, and retry, until we succeed.
	for s.doc.Life == Alive {
		log.Debugf("state: found %d relations for service %q", s.doc.RelationCount, s)
		rels, err := s.Relations()
		if err != nil {
			return err
		}
		if len(rels) != s.doc.RelationCount {
			log.Debugf("state: service %q relations changed; retrying", s)
			if err := s.Refresh(); err != nil {
				return err
			}
			continue
		}
		ops := []txn.Op{{
			C:      s.st.services.Name,
			Id:     s.doc.Name,
			Assert: D{{"life", Alive}, {"txn-revno", s.doc.TxnRevno}},
			Update: D{{"$set", D{{"life", Dying}}}},
		}}
		for _, rel := range rels {
			if rel.Life() == Alive {
				ops = append(ops, txn.Op{
					C:      s.st.relations.Name,
					Id:     rel.doc.Key,
					Assert: isAlive,
					Update: D{{"$set", D{{"life", Dying}}}},
				})
			}
		}
		if err := s.st.runner.Run(ops, "", nil); err == txn.ErrAborted {
			log.Debugf("state: service %q changed, retrying", s)
			if err := s.Refresh(); err != nil {
				return err
			}
			continue
		} else if err != nil {
			return err
		}
		log.Debugf("state: service %q is now dying", s)
		s.doc.Life = Dying
	}
	return nil
}

// EnsureDead sets the service lifecycle to Dead if it is Alive or Dying.
// It does nothing otherwise. It will return an error if the service still
// has units, or is still participating in relations.
func (s *Service) EnsureDead() error {
	assertOps := []txn.Op{{
		C:      s.st.services.Name,
		Id:     s.doc.Name,
		Assert: D{{"unitcount", 0}, {"relationcount", 0}},
	}}
	err := ensureDead(s.st, s.st.services, s.doc.Name, "service", assertOps, "service still has units and/or relations")
	if err != nil {
		return err
	}
	s.doc.Life = Dead
	return nil
}

// IsExposed returns whether this service is exposed. The explicitly open
// ports (with open-port) for exposed services may be accessed from machines
// outside of the local deployment network. See SetExposed and ClearExposed.
func (s *Service) IsExposed() bool {
	return s.doc.Exposed
}

// SetExposed marks the service as exposed.
// See ClearExposed and IsExposed.
func (s *Service) SetExposed() error {
	return s.setExposed(true)
}

// ClearExposed removes the exposed flag from the service.
// See SetExposed and IsExposed.
func (s *Service) ClearExposed() error {
	return s.setExposed(false)
}

func (s *Service) setExposed(exposed bool) (err error) {
	ops := []txn.Op{{
		C:      s.st.services.Name,
		Id:     s.doc.Name,
		Assert: isAlive,
		Update: D{{"$set", D{{"exposed", exposed}}}},
	}}
	if err := s.st.runner.Run(ops, "", nil); err != nil {
		return fmt.Errorf("cannot set exposed flag for service %q to %v: %v", s, exposed, onAbort(err, errNotAlive))
	}
	s.doc.Exposed = exposed
	return nil
}

// Charm returns the service's charm and whether units should upgrade to that
// charm even if they are in an error state.
func (s *Service) Charm() (ch *Charm, force bool, err error) {
	ch, err = s.st.Charm(s.doc.CharmURL)
	if err != nil {
		return nil, false, err
	}
	return ch, s.doc.ForceCharm, nil
}

// CharmURL returns the service's charm URL, and whether units should upgrade
// to the charm with that URL even if they are in an error state.
func (s *Service) CharmURL() (curl *charm.URL, force bool) {
	return s.doc.CharmURL, s.doc.ForceCharm
}

// Endpoints returns the service's currently available relation endpoints.
func (s *Service) Endpoints() (eps []Endpoint, err error) {
	ch, _, err := s.Charm()
	if err != nil {
		return nil, err
	}
	collect := func(role RelationRole, rels map[string]charm.Relation) {
		for name, rel := range rels {
			eps = append(eps, Endpoint{
				ServiceName:   s.doc.Name,
				Interface:     rel.Interface,
				RelationName:  name,
				RelationRole:  role,
				RelationScope: rel.Scope,
			})
		}
	}
	meta := ch.Meta()
	collect(RolePeer, meta.Peers)
	collect(RoleProvider, meta.Provides)
	collect(RoleRequirer, meta.Requires)
	collect(RoleProvider, map[string]charm.Relation{
		"juju-info": {
			Interface: "juju-info",
			Scope:     charm.ScopeGlobal,
		},
	})
	return eps, nil
}

// Endpoint returns the relation endpoint with the supplied name, if it exists.
func (s *Service) Endpoint(relationName string) (Endpoint, error) {
	eps, err := s.Endpoints()
	if err != nil {
		return Endpoint{}, err
	}
	for _, ep := range eps {
		if ep.RelationName == relationName {
			return ep, nil
		}
	}
	return Endpoint{}, fmt.Errorf("service %q has no %q relation", s, relationName)
}

// SetCharm changes the charm for the service. New units will be started with
// this charm, and existing units will be upgraded to use it. If force is true,
// units will be upgraded even if they are in an error state.
func (s *Service) SetCharm(ch *Charm, force bool) (err error) {
	ops := []txn.Op{{
		C:      s.st.services.Name,
		Id:     s.doc.Name,
		Assert: isAlive,
		Update: D{{"$set", D{{"charmurl", ch.URL()}, {"forcecharm", force}}}},
	}}
	if err := s.st.runner.Run(ops, "", nil); err != nil {
		return fmt.Errorf("cannot set charm for service %q: %v", s, onAbort(err, errNotAlive))
	}
	s.doc.CharmURL = ch.URL()
	s.doc.ForceCharm = force
	return nil
}

// String returns the service name.
func (s *Service) String() string {
	return s.doc.Name
}

// Refresh refreshes the contents of the Service from the underlying
// state. It returns a NotFoundError if the service has been removed.
func (s *Service) Refresh() error {
	err := s.st.services.FindId(s.doc.Name).One(&s.doc)
	if err == mgo.ErrNotFound {
		return notFound("service %q", s)
	}
	if err != nil {
		return fmt.Errorf("cannot refresh service %q: %v", s, err)
	}
	return nil
}

// newUnitName returns the next unit name.
func (s *Service) newUnitName() (string, error) {
	change := mgo.Change{Update: D{{"$inc", D{{"unitseq", 1}}}}}
	result := serviceDoc{}
	_, err := s.st.services.Find(D{{"_id", s.doc.Name}}).Apply(change, &result)
	if err != nil {
		return "", fmt.Errorf("cannot increment unit sequence: %v", err)
	}
	name := s.doc.Name + "/" + strconv.Itoa(result.UnitSeq)
	return name, nil
}

// addUnitOps returns a unique name for a new unit, and a list of txn operations
// necessary to create that unit. The principalName param must be non-empty if
// and only if s is a subordinate service. If s is a subordinate and strictSubordinates
// is true, the returned ops will assert that no unit of s is already a subordinate
// of the principal unit. The strictSubordinates param must be removed, and always
// assumed to be true, once AddUnitSubordinateTo hasbeen removed.
func (s *Service) addUnitOps(principalName string, strictSubordinates bool) (string, []txn.Op, error) {
	ch, _, err := s.Charm()
	if err != nil {
		return "", nil, err
	}
	if subordinate := ch.Meta().Subordinate; subordinate && principalName == "" {
		return "", nil, fmt.Errorf("service is subordinate")
	} else if !subordinate && principalName != "" {
		return "", nil, fmt.Errorf("service is not a subordinate")
	}
	name, err := s.newUnitName()
	if err != nil {
		return "", nil, err
	}
	udoc := &unitDoc{
		Name:      name,
		Service:   s.doc.Name,
		Life:      Alive,
		Status:    UnitPending,
		Principal: principalName,
	}
	ops := []txn.Op{{
		C:      s.st.units.Name,
		Id:     name,
		Assert: txn.DocMissing,
		Insert: udoc,
	}, {
		C:      s.st.services.Name,
		Id:     s.doc.Name,
		Assert: isAlive,
		Update: D{{"$inc", D{{"unitcount", 1}}}},
	}}
	if principalName != "" {
		assert := isAlive
		if strictSubordinates {
			assert = append(assert, bson.DocElem{
				"subordinates", D{{"$not", bson.RegEx{Pattern: "^" + s.doc.Name + "/"}}},
			})
		}
		ops = append(ops, txn.Op{
			C:      s.st.units.Name,
			Id:     principalName,
			Assert: assert,
			Update: D{{"$addToSet", D{{"subordinates", name}}}},
		})
	}
	return name, ops, nil
}

// AddUnit adds a new principal unit to the service.
func (s *Service) AddUnit() (unit *Unit, err error) {
	defer trivial.ErrorContextf(&err, "cannot add unit to service %q", s)
	name, ops, err := s.addUnitOps("", false)
	if err != nil {
		return nil, err
	}
	if err := s.st.runner.Run(ops, "", nil); err == txn.ErrAborted {
		if alive, err := getAlive(s.st.services, s.doc.Name); err != nil {
			return nil, err
		} else if !alive {
			return nil, fmt.Errorf("service is not alive")
		}
		return nil, fmt.Errorf("inconsistent state")
	} else if err != nil {
		return nil, err
	}
	return s.Unit(name)
}

// AddUnitSubordinateTo adds a new subordinate unit to the service, subordinate
// to principal. It does not verify relation state sanity or pre-existence of
// other subordinates of the same service; is deprecated; and only continues
// to exist for the convenience of certain tests, which are themselves due for
// overhaul.
func (s *Service) AddUnitSubordinateTo(principal *Unit) (unit *Unit, err error) {
	log.Printf("state: Service.AddUnitSubordinateTo is DEPRECATED; use RelationUnit.EnsureSubordinate instead")
	defer trivial.ErrorContextf(&err, "cannot add unit to service %q as a subordinate of %q", s, principal)
	ch, _, err := s.Charm()
	if err != nil {
		return nil, err
	}
	if !ch.Meta().Subordinate {
		return nil, fmt.Errorf("service is not a subordinate")
	}
	if !principal.IsPrincipal() {
		return nil, fmt.Errorf("unit is not a principal")
	}
	name, ops, err := s.addUnitOps(principal.doc.Name, false)
	if err != nil {
		return nil, err
	}
	if err = s.st.runner.Run(ops, "", nil); err == nil {
		return s.Unit(name)
	} else if err != txn.ErrAborted {
		return nil, err
	}
	if alive, err := getAlive(s.st.services, s.doc.Name); err != nil {
		return nil, err
	} else if !alive {
		return nil, fmt.Errorf("service is not alive")
	}
	if alive, err := getAlive(s.st.units, principal.doc.Name); err != nil {
		return nil, err
	} else if !alive {
		return nil, fmt.Errorf("principal unit is not alive")
	}
	return nil, fmt.Errorf("inconsistent state")
}

// RemoveUnit removes the given unit from s.
func (s *Service) RemoveUnit(u *Unit) (err error) {
	defer trivial.ErrorContextf(&err, "cannot remove unit %q", u)
	if u.doc.Life != Dead {
		return errors.New("unit is not dead")
	}
	if u.doc.Service != s.doc.Name {
		return fmt.Errorf("unit is not assigned to service %q", s)
	}
	if u.doc.MachineId != "" {
		err = u.UnassignFromMachine()
		if err != nil {
			return err
		}
	}
	ops := []txn.Op{{
		C:      s.st.units.Name,
		Id:     u.doc.Name,
		Assert: D{{"life", Dead}},
		Remove: true,
	}, {
		C:      s.st.services.Name,
		Id:     s.doc.Name,
		Assert: D{{"unitcount", D{{"$gt", 0}}}},
		Update: D{{"$inc", D{{"unitcount", -1}}}},
	}}
	if u.doc.Principal != "" {
		ops = append(ops, txn.Op{
			C:      s.st.units.Name,
			Id:     u.doc.Principal,
			Assert: txn.DocExists,
			Update: D{{"$pull", D{{"subordinates", u.doc.Name}}}},
		})
	}
	// TODO assert that subordinates are empty before deleting a principal
	// (shouldn't this already be the case before the principal is Dead?)
	if err = s.st.runner.Run(ops, "", nil); err != nil {
		// TODO Remove this once we know the logic is right:
		if c, err := s.st.units.FindId(u.doc.Name).Count(); err != nil {
			return err
		} else if c > 0 {
			return fmt.Errorf("cannot remove unit; something smells bad")
		}
		// If aborted, the unit is either dead or recreated.
		return onAbort(err, nil)
	}
	return nil
}

func (s *Service) unitDoc(name string) (*unitDoc, error) {
	udoc := &unitDoc{}
	sel := D{
		{"_id", name},
		{"service", s.doc.Name},
	}
	err := s.st.units.Find(sel).One(udoc)
	if err != nil {
		return nil, err
	}
	return udoc, nil
}

// Unit returns the service's unit with name.
func (s *Service) Unit(name string) (*Unit, error) {
	if !IsUnitName(name) {
		return nil, fmt.Errorf("%q is not a valid unit name", name)
	}
	udoc := &unitDoc{}
	sel := D{{"_id", name}, {"service", s.doc.Name}}
	if err := s.st.units.Find(sel).One(udoc); err != nil {
		return nil, fmt.Errorf("cannot get unit %q from service %q: %v", name, s.doc.Name, err)
	}
	return newUnit(s.st, udoc), nil
}

// AllUnits returns all units of the service.
func (s *Service) AllUnits() (units []*Unit, err error) {
	docs := []unitDoc{}
	err = s.st.units.Find(D{{"service", s.doc.Name}}).All(&docs)
	if err != nil {
		return nil, fmt.Errorf("cannot get all units from service %q: %v", s, err)
	}
	for i := range docs {
		units = append(units, newUnit(s.st, &docs[i]))
	}
	return units, nil
}

// Relations returns a Relation for every relation the service is in.
func (s *Service) Relations() (relations []*Relation, err error) {
	defer trivial.ErrorContextf(&err, "can't get relations for service %q", s)
	docs := []relationDoc{}
	err = s.st.relations.Find(D{{"endpoints.servicename", s.doc.Name}}).All(&docs)
	if err != nil {
		return nil, err
	}
	for _, v := range docs {
		relations = append(relations, newRelation(s.st, &v))
	}
	return relations, nil
}

// Config returns the configuration node for the service.
func (s *Service) Config() (config *Settings, err error) {
	config, err = readSettings(s.st, s.globalKey())
	if err != nil {
		return nil, fmt.Errorf("cannot get configuration of service %q: %v", s, err)
	}
	return config, nil
}
