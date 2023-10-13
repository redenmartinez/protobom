package serializers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	cdx "github.com/CycloneDX/cyclonedx-go"
	cdxformats "github.com/bom-squad/protobom/pkg/formats/cyclonedx"
	"github.com/bom-squad/protobom/pkg/native"
	"github.com/bom-squad/protobom/pkg/sbom"
	"github.com/sirupsen/logrus"
)

var _ native.Serializer = &CDX{}

const (
	stateKey state = "cyclonedx_serializer_state"
)

type (
	state string
	CDX   struct {
		version  string
		encoding string
	}
)

func NewCDX(version, encoding string) *CDX {
	return &CDX{
		version:  version,
		encoding: encoding,
	}
}

func (s *CDX) Serialize(bom *sbom.Document, _ *native.SerializeOptions, _ interface{}) (interface{}, error) {
	// Load the context with the CDX value. We initialize a context here
	// but we should get it as part of the method to capture cancelations
	// from the CLI or REST API.
	state := newSerializerCDXState()
	ctx := context.WithValue(context.Background(), stateKey, state)

	doc := cdx.NewBOM()
	doc.SerialNumber = bom.Metadata.Id
	ver, err := strconv.Atoi(bom.Metadata.Version)
	if err == nil {
		doc.Version = ver
	}

	metadata := cdx.Metadata{
		Component: &cdx.Component{},
	}

	doc.Metadata = &metadata
	doc.Metadata.Lifecycles = &[]cdx.Lifecycle{}
	doc.Components = &[]cdx.Component{}
	doc.Dependencies = &[]cdx.Dependency{}

	rootComponent, err := s.root(ctx, bom)
	if err != nil {
		return nil, fmt.Errorf("generating SBOM root component: %w", err)
	} 
	if rootComponent != nil {
		doc.Metadata.Component = rootComponent
	}
	if err := s.componentsMaps(ctx, bom); err != nil {
		return nil, err
	}

	for _, dt := range bom.Metadata.DocumentTypes {
		var lfc cdx.Lifecycle

		if dt.Type == nil {
			lfc.Name = *dt.Name
			lfc.Description = *dt.Description
		} else {
			lfc.Phase, err = sbomTypeToPhase(dt)
			if err != nil {
				return nil, err
			}
		}

		*doc.Metadata.Lifecycles = append(*doc.Metadata.Lifecycles, lfc)
	}

	if bom.Metadata != nil && len(bom.GetMetadata().GetAuthors()) > 0 {
		var authors []cdx.OrganizationalContact
		for _, bomauthor := range bom.GetMetadata().GetAuthors() {
			authors = append(authors, cdx.OrganizationalContact{
				Name:  bomauthor.Name,
				Email: bomauthor.Email,
				Phone: bomauthor.Phone,
			})
		}
		metadata.Authors = &authors
	}

	if bom.Metadata != nil && len(bom.GetMetadata().GetTools()) > 0 {
		var tools []cdx.Tool
		for _, bomtool := range bom.GetMetadata().GetTools() {
			tools = append(tools, cdx.Tool{
				Vendor:  bomtool.Vendor,
				Name:    bomtool.Name,
				Version: bomtool.Version,
			})
		}
		metadata.Tools = &tools
	}

	if bom.Metadata != nil && len(bom.GetMetadata().GetName()) > 0 {
		doc.Metadata.Component.Name = bom.GetMetadata().GetName()
	}

	deps, err := s.dependencies(ctx, bom)
	if err != nil {
		return nil, err
	}
	doc.Dependencies = &deps

	components := state.components()
	clearAutoRefs(&components)
	doc.Components = &components

	return doc, nil
}

// sbomTypeToPhase converts a SBOM document type to a CDX lifecycle phase
func sbomTypeToPhase(dt *sbom.DocumentType) (cdx.LifecyclePhase, error) {
	switch *dt.Type {
	case sbom.DocumentType_BUILD:
		return cdx.LifecyclePhaseBuild, nil
	case sbom.DocumentType_DESIGN:
		return cdx.LifecyclePhaseDesign, nil
	case sbom.DocumentType_ANALYZED:
		return cdx.LifecyclePhasePostBuild, nil
	case sbom.DocumentType_SOURCE:
		return cdx.LifecyclePhasePreBuild, nil
	case sbom.DocumentType_DECOMISSION:
		return cdx.LifecyclePhaseDecommission, nil
	case sbom.DocumentType_DEPLOYED:
		return cdx.LifecyclePhaseOperations, nil
	case sbom.DocumentType_DISCOVERY:
		return cdx.LifecyclePhaseDiscovery, nil
	case sbom.DocumentType_OTHER:
		return cdx.LifecyclePhase(strings.ToLower(*dt.Name)), nil
	}

	return "", fmt.Errorf("unknown document type %s", *dt.Name)
}

// clearAutoRefs
// The last step of the CDX serialization recursively removes all autogenerated
// refs added by the protobom reader. These are added on CycloneDX ingestion
// to all nodes that don't have them. To maintain the closest fidelity, we
// clear their refs again before output to CDX
func clearAutoRefs(comps *[]cdx.Component) {
	for i := range *comps {
		if strings.HasPrefix((*comps)[i].BOMRef, "protobom-") {
			flags := strings.Split((*comps)[i].BOMRef, "--")
			if strings.Contains(flags[0], "-auto") {
				(*comps)[i].BOMRef = ""
			}
		}
		if (*comps)[i].Components != nil && len(*(*comps)[i].Components) != 0 {
			clearAutoRefs((*comps)[i].Components)
		}
	}
}

func (s *CDX) componentsMaps(ctx context.Context, bom *sbom.Document) error {
	state, err := getCDXState(ctx)
	if err != nil {
		return fmt.Errorf("reading state: %w", err)
	}

	for _, n := range bom.NodeList.Nodes {
		comp := s.nodeToComponent(n)
		if comp == nil {
			// Error? Warn?
			continue
		}

		state.componentsDict[comp.BOMRef] = comp
	}
	return nil
}

func (s *CDX) root(ctx context.Context, bom *sbom.Document) (*cdx.Component, error) {
	var rootComp *cdx.Component
	// First, assign the top level nodes
	state, err := getCDXState(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading state: %w", err)
	}

	// 2DO Use GetRootNodes() https://github.com/bom-squad/protobom/pull/20
	if bom.NodeList.RootElements != nil && len(bom.NodeList.RootElements) > 0 {
		for _, id := range bom.NodeList.RootElements {
			// Search for the node and add it
			for _, n := range bom.NodeList.Nodes {
				if n.Id == id {
					rootComp = s.nodeToComponent(n)
					state.addedDict[id] = struct{}{}
				}
			}

			// TODO(degradation): Here we would document other root level elements
			// are not added to to document
			if true { // temp workaround in favor of adding a lint tag
				break
			}
		}
	}

	return rootComp, nil
}

// NOTE dependencies function modifies the components dictionary
func (s *CDX) dependencies(ctx context.Context, bom *sbom.Document) ([]cdx.Dependency, error) {
	var dependencies []cdx.Dependency
	state, err := getCDXState(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading state: %w", err)
	}

	for _, e := range bom.NodeList.Edges {
		e := e
		if _, ok := state.addedDict[e.From]; ok {
			continue
		}

		if _, ok := state.componentsDict[e.From]; !ok {
			logrus.Info("serialize")
			return nil, fmt.Errorf("unable to find component %s", e.From)
		}

		// In this example, we tree-ify all components related with a
		// "contains" relationship. This is just an opinion for the demo
		// and it is something we can parameterize
		switch e.Type {
		case sbom.Edge_contains:
			// Make sure we have the target component
			for _, targetID := range e.To {
				state.addedDict[targetID] = struct{}{}
				if _, ok := state.componentsDict[targetID]; !ok {
					return nil, fmt.Errorf("unable to locate node %s", targetID)
				}

				if state.componentsDict[e.From].Components == nil {
					state.componentsDict[e.From].Components = &[]cdx.Component{}
				}
				*state.componentsDict[e.From].Components = append(*state.componentsDict[e.From].Components, *state.componentsDict[targetID])
			}

		case sbom.Edge_dependsOn:
			// Add to the dependency tree
			targetStrings := []string{}
			depListCheck := map[string]struct{}{}
			for _, targetID := range e.To {
				// Add entries to dependency only once.
				if _, ok := depListCheck[targetID]; ok {
					continue
				}

				if _, ok := state.componentsDict[targetID]; !ok {
					return nil, fmt.Errorf("unable to locate node %s", targetID)
				}

				state.addedDict[targetID] = struct{}{}
				depListCheck[targetID] = struct{}{}
				targetStrings = append(targetStrings, targetID)
			}
			dependencies = append(dependencies, cdx.Dependency{
				Ref:          e.From,
				Dependencies: &targetStrings,
			})
		default:
			// TODO(degradation) here, we would document how relationships are lost
			logrus.Warnf(
				"node %s is related with %s to %d other nodes, data will be lost",
				e.From, e.Type, len(e.To),
			)
		}
	}

	return dependencies, nil
}

// nodeToComponent converts a node in protobuf to a CycloneDX component
func (s *CDX) nodeToComponent(n *sbom.Node) *cdx.Component {
	if n == nil {
		return nil
	}
	c := &cdx.Component{
		BOMRef:      n.Id,
		Name:        n.Name,
		Version:     n.Version,
		Description: n.Description,
	}

	if n.Type == sbom.Node_FILE {
		c.Type = cdx.ComponentTypeFile
	} else {
		switch strings.ToLower(n.PrimaryPurpose) {
		case "application":
			c.Type = cdx.ComponentTypeApplication
		case "container":
			c.Type = cdx.ComponentTypeContainer
		case "data":
			c.Type = cdx.ComponentTypeData
		case "device":
			c.Type = cdx.ComponentTypeDevice
		case "device-driver":
			c.Type = cdx.ComponentTypeDeviceDriver
		case "file":
			c.Type = cdx.ComponentTypeFile
		case "firmware":
			c.Type = cdx.ComponentTypeFirmware
		case "framework":
			c.Type = cdx.ComponentTypeFramework
		case "library":
			c.Type = cdx.ComponentTypeLibrary
		case "machine-learning-model":
			c.Type = cdx.ComponentTypeMachineLearningModel
		case "operating-system":
			c.Type = cdx.ComponentTypeOS
		case "platform":
			c.Type = cdx.ComponentTypePlatform
		case "":
			// no node PrimaryPurpose set
		default:
			// TODO(degradation): Non-matching primary purpose to component type mapping
		}
	}

	if n.Licenses != nil && len(n.Licenses) > 0 {
		var licenseChoices []cdx.LicenseChoice
		var licenses cdx.Licenses
		for _, l := range n.Licenses {
			licenseChoices = append(licenseChoices, cdx.LicenseChoice{
				License: &cdx.License{
					ID: l,
				},
			})
		}

		licenses = licenseChoices
		c.Licenses = &licenses
	}

	if n.Hashes != nil && len(n.Hashes) > 0 {
		c.Hashes = &[]cdx.Hash{}
		for algo, hash := range n.Hashes {
			ha := sbom.HashAlgorithm(algo)
			if ha.ToCycloneDX() == "" {
				// TODO(degradation): Algorithm not supported in CDX
				continue
			}
			*c.Hashes = append(*c.Hashes, cdx.Hash{
				Algorithm: ha.ToCycloneDX(),
				Value:     hash,
			})
		}
	}

	if n.ExternalReferences != nil {
		for _, er := range n.ExternalReferences {
			if c.ExternalReferences == nil {
				c.ExternalReferences = &[]cdx.ExternalReference{}
			}

			*c.ExternalReferences = append(*c.ExternalReferences, cdx.ExternalReference{
				Type: cdx.ExternalReferenceType(er.Type), // Fix to make it valid
				URL:  er.Url,
			})
		}
	}

	if n.Identifiers != nil {
		for idType := range n.Identifiers {
			switch idType {
			case int32(sbom.SoftwareIdentifierType_PURL):
				c.PackageURL = n.Identifiers[idType]
			case int32(sbom.SoftwareIdentifierType_CPE23):
				c.CPE = n.Identifiers[idType]
			case int32(sbom.SoftwareIdentifierType_CPE22):
				// TODO(degradation): Only one CPE is supperted in CDX
				if c.CPE == "" {
					c.CPE = n.Identifiers[idType]
				}
			}
		}
	}

	if n.Suppliers != nil && len(n.GetSuppliers()) > 0 {
		// TODO(degradation): CDX type Component only supports one Supplier while protobom supports multiple

		nodesupplier := n.GetSuppliers()[0]
		oe := cdx.OrganizationalEntity{
			Name: nodesupplier.GetName(),
		}
		if nodesupplier.Contacts != nil {
			var contacts []cdx.OrganizationalContact
			for _, nodecontact := range nodesupplier.GetContacts() {
				newcontact := cdx.OrganizationalContact{
					Name:  nodecontact.GetName(),
					Email: nodecontact.GetEmail(),
					Phone: nodecontact.GetPhone(),
				}
				contacts = append(contacts, newcontact)
			}
			oe.Contact = &contacts
		}
		c.Supplier = &oe
	}

	if len(n.GetCopyright()) > 0 {
		c.Copyright = n.GetCopyright()
	}

	return c
}

// Render calls the official CDX serializer to render the BOM into a specific version
func (s *CDX) Render(doc interface{}, wr io.Writer, o *native.RenderOptions, _ interface{}) error {
	if doc == nil {
		return errors.New("document is nil")
	}

	version, err := cdxformats.ParseVersion(s.version)
	if err != nil {
		return fmt.Errorf("getting CDX version: %w", err)
	}

	encoding, err := cdxformats.ParseEncoding(s.encoding)
	if err != nil {
		return fmt.Errorf("getting CDX encoding: %w", err)
	}

	encoder := cdx.NewBOMEncoder(wr, encoding)
	encoder.SetPretty(true)

	if _, ok := doc.(*cdx.BOM); !ok {
		return errors.New("document is not a cyclonedx bom")
	}

	if err := encoder.EncodeVersion(doc.(*cdx.BOM), version); err != nil {
		return fmt.Errorf("encoding sbom to stream: %w", err)
	}

	return nil
}

type serializerCDXState struct {
	addedDict      map[string]struct{}
	componentsDict map[string]*cdx.Component
}

func newSerializerCDXState() *serializerCDXState {
	return &serializerCDXState{
		addedDict:      map[string]struct{}{},
		componentsDict: map[string]*cdx.Component{},
	}
}

func (s *serializerCDXState) components() []cdx.Component {
	components := []cdx.Component{}
	for _, c := range s.componentsDict {
		if _, ok := s.addedDict[c.BOMRef]; ok {
			continue
		}
		components = append(components, *c)
	}

	return components
}

func getCDXState(ctx context.Context) (*serializerCDXState, error) {
	dm, ok := ctx.Value(stateKey).(*serializerCDXState)
	if !ok {
		return nil, errors.New("unable to cast serializer state from context")
	}
	return dm, nil
}
