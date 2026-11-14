# dmgbuild settings for Install-Switcher.dmg; build.sh passes the paths with
# -D. The two icon places match the .slot marks in background.html.
# pylint: disable=undefined-variable
app = defines["app"]  # noqa: F821
files = [app]
symlinks = {"Privacy & Security": "/System/Library/PreferencePanes/Security.prefPane"}
hide_extension = ["Install Switcher.app"]
icon = defines["icon"]  # noqa: F821
background = defines["background"]  # noqa: F821
format = "ULFO"
filesystem = "HFS+"
size = None
show_status_bar = False
show_tab_view = False
show_toolbar = False
show_pathbar = False
show_sidebar = False
show_icon_preview = False
default_view = "icon-view"
include_icon_view_settings = True
include_list_view_settings = False
# The size includes the title bar: 440 points of content, as background.html.
window_rect = ((200, 140), (720, 468))
arrange_by = None
grid_offset = (0, 0)
grid_spacing = 100
scroll_position = (0, 0)
label_pos = "bottom"
text_size = 13
icon_size = 112
icon_locations = {
    "Install Switcher.app": (186, 200),
    "Privacy & Security": (534, 200),
}
