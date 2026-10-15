// Package events holds yc-image's event subscribers.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	ycevents "github.com/slaghuis/yc-platform/libs/yc-events"
)

// Processor renders an uploaded image's variants.
type Processor interface {
	Process(ctx context.Context, tenantID, imageID string) error
}

// UploadedSubscription is yc-image's consumer of its own uploads. Rendering
// a large image takes a while, so the ack deadline is generous.
var UploadedSubscription = eventbus.SubscribeOpts{
	Subject:     "yc.*." + ycevents.TypeImageUploadedV1,
	DurableName: "yc-image-processor",
	AckWait:     2 * time.Minute,
}

// Uploaded processes an accepted upload.
func Uploaded(p Processor) eventbus.Handler {
	return func(ctx context.Context, evt eventbus.ConsumedEvent) error {
		var payload ycevents.ImageUploadedV1
		if err := json.Unmarshal(evt.Payload, &payload); err != nil {
			return fmt.Errorf("%w: decode payload: %w", eventbus.ErrPoison, err)
		}
		return p.Process(ctx, evt.Envelope.TenantID, payload.ImageID)
	}
}
