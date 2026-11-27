package cot

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"time"
)

// Version is the CoT schema version every event this project writes declares.
const Version = "2.0"

// Unknown is the CoT sentinel for a value nobody knows, used for hae (height), ce (circular error) and le (linear
// error) when the source has no estimate.
const Unknown Number = 9999999.0

// TimeLayout is the timestamp format CoT uses: UTC with millisecond precision, for example 2026-11-27T16:00:00.000Z.
const TimeLayout = "2006-01-02T15:04:05.000Z"

// Event is one Cursor-on-Target event: what a thing is (Type), which thing it is (UID), when the report is valid (Time,
// Start, Stale), how the position was derived (How), where it is (Point), and anything else (Detail).
type Event struct {
	XMLName xml.Name  `xml:"event"`
	Version string    `xml:"version,attr"`
	UID     string    `xml:"uid,attr"`
	Type    string    `xml:"type,attr"`
	Time    Timestamp `xml:"time,attr"`
	Start   Timestamp `xml:"start,attr"`
	Stale   Timestamp `xml:"stale,attr"`
	How     string    `xml:"how,attr"`
	Point   Point     `xml:"point"`
	Detail  *Detail   `xml:"detail,omitempty"`
}

// Point is the event's position: latitude and longitude in degrees (WGS84), hae is height above the ellipsoid in
// meters, and ce and le are the circular and linear error estimates in meters.
type Point struct {
	Lat Number `xml:"lat,attr"`
	Lon Number `xml:"lon,attr"`
	HAE Number `xml:"hae,attr"`
	CE  Number `xml:"ce,attr"`
	LE  Number `xml:"le,attr"`
}

// Detail holds the optional sub-elements this project reads and writes. Each one is a pointer so that an absent
// element is left out of the XML instead of being written empty.
type Detail struct {
	Contact *Contact `xml:"contact,omitempty"`
	Group   *Group   `xml:"__group,omitempty"`
	Track   *Track   `xml:"track,omitempty"`
	Remarks *Remarks `xml:"remarks,omitempty"`
}

// Contact carries the human-readable name the map shows for the thing.
type Contact struct {
	Callsign string `xml:"callsign,attr"`
}

// Group is the TAK team (a color name, such as "Cyan") and role, which TAK clients use to color the marker.
type Group struct {
	Name string `xml:"name,attr"`
	Role string `xml:"role,attr"`
}

// Track is the direction of travel in degrees from true north and the speed in meters per second.
type Track struct {
	Course Number `xml:"course,attr"`
	Speed  Number `xml:"speed,attr"`
}

// Remarks is free text attached to the event.
type Remarks struct {
	Text string `xml:",chardata"`
}

// Number is a float64 that always marshals as a plain decimal. encoding/xml would otherwise format floats with
// strconv's 'g' verb, which writes the Unknown sentinel as 9.999999e+06, and not every CoT reader accepts exponents.
type Number float64

// MarshalXMLAttr implements xml.MarshalerAttr.
func (n Number) MarshalXMLAttr(name xml.Name) (xml.Attr, error) {
	return xml.Attr{Name: name, Value: strconv.FormatFloat(float64(n), 'f', -1, 64)}, nil
}

// UnmarshalXMLAttr implements xml.UnmarshalerAttr.
func (n *Number) UnmarshalXMLAttr(attr xml.Attr) error {
	v, err := strconv.ParseFloat(attr.Value, 64)
	if err != nil {
		return fmt.Errorf("attribute %s: %w", attr.Name.Local, err)
	}
	*n = Number(v)
	return nil
}

// Timestamp is a time.Time that marshals in TimeLayout. The default encoding (RFC 3339 with a variable-length
// fraction) is valid too, but fixed milliseconds is what CoT producers conventionally write.
type Timestamp time.Time

// MarshalXMLAttr implements xml.MarshalerAttr.
func (t Timestamp) MarshalXMLAttr(name xml.Name) (xml.Attr, error) {
	return xml.Attr{Name: name, Value: time.Time(t).UTC().Format(TimeLayout)}, nil
}

// UnmarshalXMLAttr implements xml.UnmarshalerAttr. It accepts any RFC 3339 timestamp, which includes TimeLayout.
func (t *Timestamp) UnmarshalXMLAttr(attr xml.Attr) error {
	v, err := time.Parse(time.RFC3339Nano, attr.Value)
	if err != nil {
		return fmt.Errorf("attribute %s: %w", attr.Name.Local, err)
	}
	*t = Timestamp(v)
	return nil
}
