package structurizr

import (
	"encoding/json"
	"fmt"
)

type jsonRelationship struct {
	SourceID             string `json:"sourceId"`
	DestinationID        string `json:"destinationId"`
	Description          string `json:"description"`
	Technology           string `json:"technology"`
	LinkedRelationshipID string `json:"linkedRelationshipId"`
}

type jsonElement struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Description   string             `json:"description"`
	Technology    string             `json:"technology"`
	Tags          string             `json:"tags"`
	Properties    map[string]string  `json:"properties"`
	Relationships []jsonRelationship `json:"relationships"`
	Containers    []jsonElement      `json:"containers"`
	Components    []jsonElement      `json:"components"`
}

type jsonWorkspace struct {
	Name  string `json:"name"`
	Model struct {
		People          []jsonElement `json:"people"`
		SoftwareSystems []jsonElement `json:"softwareSystems"`
		CustomElements  []jsonElement `json:"customElements"`
		DeploymentNodes []any         `json:"deploymentNodes"`
	} `json:"model"`
}

// ParseJSON reads a Structurizr JSON workspace.
//
// An implied relationship (one that carries linkedRelationshipId) is skipped:
// it restates the explicit relationship it was derived from.
func ParseJSON(data []byte) (*Workspace, error) {
	var document jsonWorkspace
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, &Error{File: "workspace.json", Message: fmt.Sprintf("not a Structurizr JSON workspace: %v", err)}
	}
	workspace := &Workspace{Name: document.Name}
	byID := map[string]*Element{}
	var pending []jsonRelationship

	var add func(source jsonElement, kind Kind, parent *Element) error
	add = func(source jsonElement, kind Kind, parent *Element) error {
		if source.ID == "" {
			return &Error{File: "workspace.json", Message: fmt.Sprintf("element %q has no id", source.Name)}
		}
		if _, dup := byID[source.ID]; dup {
			return &Error{File: "workspace.json", Message: fmt.Sprintf("element id %q is declared twice", source.ID)}
		}
		element := &Element{
			Ref:         source.ID,
			Kind:        kind,
			Name:        source.Name,
			Description: source.Description,
			Technology:  source.Technology,
			Tags:        splitTags(source.Tags),
			Properties:  source.Properties,
			Parent:      parent,
		}
		if parent != nil {
			parent.Children = append(parent.Children, element)
		}
		byID[source.ID] = element
		workspace.Elements = append(workspace.Elements, element)
		pending = append(pending, source.Relationships...)
		for _, child := range source.Containers {
			if err := add(child, KindContainer, element); err != nil {
				return err
			}
		}
		for _, child := range source.Components {
			if err := add(child, KindComponent, element); err != nil {
				return err
			}
		}
		return nil
	}
	for _, person := range document.Model.People {
		if err := add(person, KindPerson, nil); err != nil {
			return nil, err
		}
	}
	for _, system := range document.Model.SoftwareSystems {
		if err := add(system, KindSoftwareSystem, nil); err != nil {
			return nil, err
		}
	}
	for _, custom := range document.Model.CustomElements {
		if err := add(custom, KindOther, nil); err != nil {
			return nil, err
		}
	}
	for _, relationship := range pending {
		if relationship.LinkedRelationshipID != "" {
			continue
		}
		source, destination := byID[relationship.SourceID], byID[relationship.DestinationID]
		if source == nil || destination == nil {
			// A relationship of a deployment element: not part of the static model.
			continue
		}
		workspace.Relationships = append(workspace.Relationships, Relationship{
			Source:      source,
			Destination: destination,
			Description: relationship.Description,
			Technology:  relationship.Technology,
		})
	}
	return workspace, nil
}
