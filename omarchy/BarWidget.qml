import QtQuick
import Quickshell
import Quickshell.Io
import qs.Commons
import qs.Ui

// Indicador de compilaciones de Rust. Lee `turn_watch status` (una línea JSON
// que escribe el daemon de usuario turn_watch.service) cada 2 s. Se ve solo
// mientras hay compilaciones; el tooltip trae el detalle y el clic abre
// el panel de turn_watch en una terminal flotante.
BarWidget {
  id: root
  moduleName: "turn_watch"

  property int activeCount: 0
  property real cores: 0
  property string tooltip: ""
  readonly property bool building: activeCount > 0

  implicitWidth: indicator.implicitWidth
  implicitHeight: indicator.implicitHeight

  function update(raw) {
    var data = indicator.extractData(raw)
    root.activeCount = Number(data.active || 0)
    root.cores = Number(data.cores || 0)
    // Texto propio del daemon local; se muestra como texto plano en el tooltip.
    root.tooltip = String(data.tooltip || "").slice(0, 2000)
  }

  BarIndicator {
    id: indicator
    anchors.fill: parent
    moduleName: root.moduleName
    settings: root.settings
    active: root.building
    activeColor: Color.accent
    activeText: ""
    inactiveText: ""
    activeTooltipText: root.tooltip
    inactiveTooltipText: root.tooltip
    // Solo ocupa lugar mientras compila algo.
    revealWhenInactive: false
    onPressed: function(button) {
      Quickshell.execDetached(["omarchy-launch-tui", "--app-id=org.omarchy.terminal", Quickshell.env("HOME") + "/.local/bin/turn_watch"])
    }
  }

  // Número de compilaciones a la vez, en una esquina, si hay más de una.
  Text {
    visible: root.activeCount > 1
    textFormat: Text.PlainText
    text: String(root.activeCount)
    color: Color.accent
    font.pixelSize: Style.space(9)
    font.bold: true
    anchors.right: parent.right
    anchors.top: parent.top
    anchors.rightMargin: Style.space(2)
  }

  Timer {
    interval: 2000
    running: true
    repeat: true
    triggeredOnStart: true
    onTriggered: if (!statusProc.running) statusProc.running = true
  }

  Process {
    id: statusProc
    command: [Quickshell.env("HOME") + "/.local/bin/turn_watch", "status"]
    stdout: StdioCollector {
      waitForEnd: true
      onStreamFinished: root.update(text)
    }
  }
}
