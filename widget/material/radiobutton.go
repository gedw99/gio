// SPDX-License-Identifier: Unlicense OR MIT

package material

import (
	"gioui.org/io/semantic"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
)

type RadioButtonStyle struct {
	checkable
	Key   string
	Group *widget.Enum
}

// RadioButton returns a RadioButton with a label. The key specifies
// the value for the Enum.
func RadioButton(th *Theme, group *widget.Enum, key, label string) RadioButtonStyle {
	return RadioButtonStyle{
		Group: group,
		checkable: checkable{
			Label: label,

			Color:              th.Palette.Fg,
			IconColor:          th.Palette.ContrastBg,
			TextSize:           th.TextSize.Scale(14.0 / 16.0),
			Size:               unit.Dp(26),
			shaper:             th.Shaper,
			checkedStateIcon:   th.Icon.RadioChecked,
			uncheckedStateIcon: th.Icon.RadioUnchecked,
		},
		Key: key,
	}
}

// Layout updates enum and displays the radio button.
func (r RadioButtonStyle) Layout(gtx layout.Context) layout.Dimensions {
	hovered, hovering := r.Group.Hovered()
	return r.Group.Layout(gtx, r.Key, func(gtx layout.Context) layout.Dimensions {
		semantic.RadioButton.Add(gtx.Ops)
		return r.layout(gtx, r.Group.Value == r.Key, hovering && hovered == r.Key)
	})
}
