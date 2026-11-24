# Graph Explorer navigation review

This focused contract covers the graph zoom, pan, and focus controls. The main
surface and data-state contract remains in `graph-explorer-contract.md`.

- **User job:** enlarge dense graph neighborhoods and move around them without
  losing the selected route or misreading the graph as complete.
- **Primary actions:** zoom with toolbar/wheel/keyboard, drag to pan, use
  **Вписать** to reset, and toggle the graph focus mode.
- **Target viewport:** desktop browser viewport `1280x720`.
- **Safety:** this view is read-only; URL parameter values and raw HTTP bodies
  remain hidden.
- **Partial state:** graph coverage can remain partial while the view is
  zoomed, panned, or expanded.
- **Keyboard return:** Escape restores the endpoint and inspector panels from
  focus mode.

