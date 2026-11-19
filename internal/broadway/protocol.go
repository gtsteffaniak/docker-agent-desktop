// Package broadway implements the GTK 3 Broadway WebSocket client protocol
// for input injection and framebuffer decoding .
package broadway

// EventType values match gdk/broadway/broadway-protocol.h (client → server).
const (
	EventEnter         = 'e'
	EventLeave         = 'l'
	EventPointerMove   = 'm'
	EventButtonPress   = 'b'
	EventButtonRelease = 'B'
	EventScroll        = 's'
	EventKeyPress      = 'k'
	EventKeyRelease    = 'K'
	EventScreenSize    = 'd'
	// EventConfigureNotify confirms a surface's position and size back to GDK, as the browser does
	// after every move or resize. Without it an app never redraws at its new size.
	EventConfigureNotify = 'w'
	EventGrabNotify      = 'g'
	EventUngrabNotify    = 'u'
)

// GDK modifier masks used in Broadway pointer/key state.
const (
	ModShift   = 1 << 0
	ModControl = 1 << 2
	ModAlt     = 1 << 3
)

// Crossing modes (subset used by the HTML5 client).
const (
	CrossingNormal = 0
)
