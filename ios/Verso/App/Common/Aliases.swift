import SwiftUI
import VersoCore

/// VersoCore's thread `Binding` (cloth colour) shadows SwiftUI's in files importing both.
/// Inside the app, `Binding` means SwiftUI's; the cloth colour is `Cloth`.
typealias Binding<Value> = SwiftUI.Binding<Value>
typealias Cloth = VersoCore.Binding
