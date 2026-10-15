package domain

// ChannelAddress is one of a user's messaging channels in yc-preference.
type ChannelAddress struct {
	Channel  Channel
	Address  string
	Verified bool
}

// Contact is what yc-message knows about how to reach a recipient: a
// customer's preferences, or a guest's contact details.
type Contact struct {
	UserID   string
	Primary  Channel
	Channels []ChannelAddress
	Locale   string
}

// GuestContact is a guest's contact from their order (ADR-010).
func GuestContact(email, phone string) Contact {
	var c Contact
	if email != "" {
		c.Channels = append(c.Channels, ChannelAddress{Channel: ChannelEmail, Address: email})
	}
	if phone != "" {
		c.Channels = append(c.Channels, ChannelAddress{Channel: ChannelSMS, Address: phone})
	}
	return c
}

func (c Contact) channel(ch Channel) (ChannelAddress, bool) {
	for _, a := range c.Channels {
		if a.Channel == ch && a.Address != "" {
			return a, true
		}
	}
	return ChannelAddress{}, false
}

// Recipient is where a message goes.
type Recipient struct {
	UserID  string
	Channel Channel
	Address string
	Locale  string
}

// Resolve picks the channel a message goes to (ADR-023 rule 2): the
// primary channel when it is verified and has a driver; otherwise email,
// verified or not, because the account's email is how the shop reaches the
// customer; for a guest without an email, SMS. ok is false when there is
// no usable address.
func (c Contact) Resolve(hasDriver func(Channel) bool) (Recipient, bool) {
	r := Recipient{UserID: c.UserID, Locale: c.Locale}
	if p, ok := c.channel(c.Primary); ok && p.Verified && hasDriver(p.Channel) {
		r.Channel, r.Address = p.Channel, p.Address
		return r, true
	}
	for _, ch := range []Channel{ChannelEmail, ChannelSMS} {
		if a, ok := c.channel(ch); ok && hasDriver(ch) {
			r.Channel, r.Address = ch, a.Address
			return r, true
		}
	}
	return Recipient{}, false
}
