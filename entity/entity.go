// Package entity is a small, SDK-free model of the Lattice entity fields this project produces and checks.
//
// Field names and JSON tags follow the Lattice REST API, so an Entity marshals to the same JSON shape the API uses,
// which is also what `takgw preflight` reads. Only the fields the gateway sets are modeled, and every field is a
// pointer with omitempty, because a missing field and a zero value mean different things to the server. The official
// SDK is used only at the edge: the publish adapter converts an Entity into the SDK's types.
package entity

import "time"

// Disposition is milView.disposition: the affiliation of the thing.
type Disposition string

// The dispositions the mapper emits. Entity Preflight checks any value against the installed SDK, so this list is
// only what this project writes, not a copy of the API's enum.
const (
	DispositionUnknown         Disposition = "DISPOSITION_UNKNOWN"
	DispositionFriendly        Disposition = "DISPOSITION_FRIENDLY"
	DispositionHostile         Disposition = "DISPOSITION_HOSTILE"
	DispositionSuspicious      Disposition = "DISPOSITION_SUSPICIOUS"
	DispositionAssumedFriendly Disposition = "DISPOSITION_ASSUMED_FRIENDLY"
	DispositionNeutral         Disposition = "DISPOSITION_NEUTRAL"
	DispositionPending         Disposition = "DISPOSITION_PENDING"
)

// Environment is milView.environment: where the thing operates.
type Environment string

// The environments the mapper emits.
const (
	EnvironmentUnknown    Environment = "ENVIRONMENT_UNKNOWN"
	EnvironmentAir        Environment = "ENVIRONMENT_AIR"
	EnvironmentSurface    Environment = "ENVIRONMENT_SURFACE"
	EnvironmentSubSurface Environment = "ENVIRONMENT_SUB_SURFACE"
	EnvironmentLand       Environment = "ENVIRONMENT_LAND"
	EnvironmentSpace      Environment = "ENVIRONMENT_SPACE"
)

// Template is ontology.template: the broad kind of entity.
type Template string

// TemplateTrack is the template for things observed or reported on the map, which is what CoT position reports are.
const TemplateTrack Template = "TEMPLATE_TRACK"

// Entity is one candidate entity.
type Entity struct {
	EntityID            *string              `json:"entityId,omitempty"`
	IsLive              *bool                `json:"isLive,omitempty"`
	ExpiryTime          *time.Time           `json:"expiryTime,omitempty"`
	Aliases             *Aliases             `json:"aliases,omitempty"`
	MilView             *MilView             `json:"milView,omitempty"`
	Ontology            *Ontology            `json:"ontology,omitempty"`
	Location            *Location            `json:"location,omitempty"`
	LocationUncertainty *LocationUncertainty `json:"locationUncertainty,omitempty"`
	Symbology           *Symbology           `json:"symbology,omitempty"`
	Provenance          *Provenance          `json:"provenance,omitempty"`
	Indicators          *Indicators          `json:"indicators,omitempty"`
	Tracked             *Tracked             `json:"tracked,omitempty"`
}

// Aliases holds the display name operators see.
type Aliases struct {
	Name *string `json:"name,omitempty"`
}

// MilView is the military view of the entity: affiliation and environment.
type MilView struct {
	Disposition *Disposition `json:"disposition,omitempty"`
	Environment *Environment `json:"environment,omitempty"`
}

// Ontology is what kind of entity this is.
type Ontology struct {
	Template     *Template `json:"template,omitempty"`
	PlatformType *string   `json:"platformType,omitempty"`
}

// Location is the entity's position and, if known, its velocity.
type Location struct {
	Position    *Position `json:"position,omitempty"`
	VelocityEnu *ENU      `json:"velocityEnu,omitempty"`
}

// Position is a WGS84 position, with height above the ellipsoid in meters.
type Position struct {
	LatitudeDegrees   *float64 `json:"latitudeDegrees,omitempty"`
	LongitudeDegrees  *float64 `json:"longitudeDegrees,omitempty"`
	AltitudeHaeMeters *float64 `json:"altitudeHaeMeters,omitempty"`
}

// ENU is a vector in local east, north and up components; for velocity, in meters per second.
type ENU struct {
	E *float64 `json:"e,omitempty"`
	N *float64 `json:"n,omitempty"`
	U *float64 `json:"u,omitempty"`
}

// LocationUncertainty describes how far off the position may be.
type LocationUncertainty struct {
	PositionErrorEllipse *ErrorEllipse `json:"positionErrorEllipse,omitempty"`
}

// ErrorEllipse is an uncertainty ellipse around the position, with axes in meters and orientation in degrees.
type ErrorEllipse struct {
	Probability    *float64 `json:"probability,omitempty"`
	SemiMajorAxisM *float64 `json:"semiMajorAxisM,omitempty"`
	SemiMinorAxisM *float64 `json:"semiMinorAxisM,omitempty"`
	OrientationD   *float64 `json:"orientationD,omitempty"`
}

// Symbology is how the entity is drawn.
type Symbology struct {
	MilStd2525C *MilStd2525C `json:"milStd2525C,omitempty"`
}

// MilStd2525C carries a 15-character MIL-STD-2525C symbol identification code.
type MilStd2525C struct {
	SIDC *string `json:"sidc,omitempty"`
}

// Provenance says where the data came from and when the source last updated it.
type Provenance struct {
	IntegrationName   *string    `json:"integrationName,omitempty"`
	DataType          *string    `json:"dataType,omitempty"`
	SourceID          *string    `json:"sourceId,omitempty"`
	SourceUpdateTime  *time.Time `json:"sourceUpdateTime,omitempty"`
	SourceDescription *string    `json:"sourceDescription,omitempty"`
}

// Indicators are flags about the entity.
type Indicators struct {
	Simulated *bool `json:"simulated,omitempty"`
	Exercise  *bool `json:"exercise,omitempty"`
	Emergency *bool `json:"emergency,omitempty"`
}

// Tracked holds track-specific fields.
type Tracked struct {
	TrackQualityWrapper *int `json:"trackQualityWrapper,omitempty"`
}
