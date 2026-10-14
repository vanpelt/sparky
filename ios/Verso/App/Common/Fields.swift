import SwiftUI
import UIKit
import VersoCore

/// A text line set on paper: label in small caps, his words in Iowan italic, a hairline beneath.
struct PaperField: View {
    let label: String?
    @Binding var text: String
    var prompt: String = ""
    var italic: Bool = true
    var axis: Axis = .horizontal
    /// The field's accessibility identifier, and the key it keeps the keyboard by across a fold (`keepsFocus`).
    var id: String? = nil
    var onCommit: () -> Void = {}
    @FocusState private var focused: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            if let label { Text(label).runningHeadStyle(size: 10) }
            TextField("", text: $text, prompt: Text(prompt).foregroundStyle(Palette.fadedInk.opacity(0.7)), axis: axis)
                .font(italic ? VersoFont.his(18) : VersoFont.body(17))
                .foregroundStyle(Palette.ink)
                // A vertical-axis TextField with no line range collapses to zero height on iOS 27:
                // keyboard up, text stored, nothing (not even the caret) drawn. The range sizes it.
                .lineLimit(1...6)
                .focused($focused)
                .accessibilityIdentifier(id ?? "")
                .submitLabel(.done)
                .onSubmit(onCommit)
                .returnEndsEditing(text: $text, focused: $focused)
                .onChange(of: focused) { _, f in if !f { onCommit() } }
                .keepsFocus(id, $focused)
            Hairline(focused ? Palette.oxblood : Palette.rule)
        }
    }
}

extension View {
    /// A vertical-axis TextField wraps like a paragraph but treats the keyboard's ✓ as a newline and never
    /// fires onSubmit, which left no way to finish a line or put the keyboard away. Every multi-line field
    /// here holds one sentence, so a typed (or pasted) newline means "done": strip it and drop focus, and
    /// the field's own focus-loss path commits.
    func returnEndsEditing(text: Binding<String>, focused: FocusState<Bool>.Binding) -> some View {
        onChange(of: text.wrappedValue) { _, new in
            guard new.contains(where: \.isNewline) else { return }
            text.wrappedValue = new.split(whereSeparator: \.isNewline).joined(separator: " ")
                .trimmingCharacters(in: .whitespaces)
            focused.wrappedValue = false
        }
    }
}

/// A numeric entry on a leader row: "Revest shares ........ 100,000".
struct NumberRow: View {
    let label: String
    @Binding var value: Double
    var format: NumberRowFormat = .plain
    var suffix: String? = nil

    enum NumberRowFormat { case plain, money, percent, decimal }

    var body: some View {
        LeaderRow {
            Text(label).font(VersoFont.body(16)).foregroundStyle(Palette.ink)
        } trailing: {
            HStack(spacing: 2) {
                field
                    .accessibilityIdentifier("number:\(label)")
                    .font(VersoFont.ledger(.body))
                    .multilineTextAlignment(.trailing)
                    .foregroundStyle(Palette.oxblood)
                    .fixedSize()
                if let suffix { Text(suffix).font(VersoFont.body(14)).foregroundStyle(Palette.fadedInk) }
            }
        }
        .padding(.vertical, 4)
    }

    @ViewBuilder var field: some View {
        switch format {
        case .plain: TextField("0", value: $value, format: .number.grouping(.automatic).precision(.fractionLength(0...2))).keyboardType(.decimalPad)
        case .money: TextField("$0", value: $value, format: .currency(code: "USD").precision(.fractionLength(0))).keyboardType(.decimalPad)
        case .percent: TextField("0%", value: $value, format: .percent.precision(.fractionLength(0...2))).keyboardType(.decimalPad)
        case .decimal: TextField("0", value: $value, format: .number.precision(.fractionLength(0...4))).keyboardType(.decimalPad)
        }
    }
}

/// An optional money row (Enough Line, override).
struct OptionalMoneyRow: View {
    let label: String
    @Binding var value: Double?
    var suffix: String? = nil
    var body: some View {
        LeaderRow {
            Text(label).font(VersoFont.body(16)).foregroundStyle(Palette.ink)
        } trailing: {
            HStack(spacing: 2) {
                TextField("—", value: $value, format: .currency(code: "USD").precision(.fractionLength(0...2)))
                    .keyboardType(.decimalPad)
                    .font(VersoFont.ledger(.body)).multilineTextAlignment(.trailing)
                    .foregroundStyle(Palette.oxblood).fixedSize()
                if let suffix { Text(suffix).font(VersoFont.body(14)).foregroundStyle(Palette.fadedInk) }
            }
        }
        .padding(.vertical, 4)
    }
}

/// A switch set as a sentence with a small-caps state word.
struct PaperToggle: View {
    let label: String
    @Binding var isOn: Bool
    var body: some View {
        Toggle(isOn: $isOn) {
            Text(label).font(VersoFont.body(16)).foregroundStyle(Palette.ink)
        }
        .tint(Palette.oxblood)
        .padding(.vertical, 2)
    }
}

/// Two-option text toggle: "after tax | pre-tax". Small caps; the chosen side is underlined in oxblood.
struct TextToggle<T: Hashable>: View {
    let options: [(T, String)]
    @Binding var selection: T
    var body: some View {
        HStack(spacing: 14) {
            ForEach(Array(options.enumerated()), id: \.offset) { i, opt in
                if i > 0 { Text("|").runningHeadStyle(Palette.rule, size: 12) }
                Button {
                    withAnimation(.smooth(duration: 0.3)) { selection = opt.0 }
                } label: {
                    Text(opt.1)
                        .runningHeadStyle(selection == opt.0 ? Palette.ink : Palette.fadedInk, size: 12)
                        .padding(.vertical, 6)
                        .overlay(alignment: .bottom) {
                            if selection == opt.0 { Rectangle().fill(Palette.oxblood).frame(height: 1) }
                        }
                }
                .buttonStyle(.plain)
                .accessibilityAddTraits(selection == opt.0 ? .isSelected : [])
            }
        }
        .sensoryFeedback(.selection, trigger: selection)
    }
}

/// A chip: small caps in a hairline capsule; filled with ink when on.
struct Chip: View {
    let title: String
    let on: Bool
    var tint: Color = Palette.oxblood
    let action: () -> Void
    var body: some View {
        Button(action: action) {
            Text(title)
                .font(VersoFont.body(15)).italic(on)
                .foregroundStyle(on ? Palette.paper : Palette.ink)
                .padding(.horizontal, 12).padding(.vertical, 6)
                .background(on ? tint : .clear, in: Capsule())
                .overlay(Capsule().strokeBorder(on ? tint : Palette.rule, lineWidth: 0.75))
        }
        .buttonStyle(.plain)
        .accessibilityAddTraits(on ? .isSelected : [])
    }
}

/// A flowing layout for chips.
struct FlowLayout: Layout {
    var spacing: CGFloat = 8
    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        let width = proposal.width ?? .infinity
        var x: CGFloat = 0, y: CGFloat = 0, rowH: CGFloat = 0, maxX: CGFloat = 0
        for v in subviews {
            let s = v.sizeThatFits(.unspecified)
            if x + s.width > width, x > 0 { x = 0; y += rowH + spacing; rowH = 0 }
            x += s.width + spacing; rowH = max(rowH, s.height); maxX = max(maxX, x)
        }
        return CGSize(width: min(width, maxX), height: y + rowH)
    }
    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        var x = bounds.minX, y = bounds.minY, rowH: CGFloat = 0
        for v in subviews {
            let s = v.sizeThatFits(.unspecified)
            if x + s.width > bounds.maxX, x > bounds.minX { x = bounds.minX; y += rowH + spacing; rowH = 0 }
            v.place(at: CGPoint(x: x, y: y), proposal: ProposedViewSize(s))
            x += s.width + spacing; rowH = max(rowH, s.height)
        }
    }
}

/// Two-finger tap (lifts the veil). SwiftUI has no multi-touch tap, so bridge UIKit.
struct TwoFingerTap: UIGestureRecognizerRepresentable {
    let action: () -> Void
    func makeUIGestureRecognizer(context: Context) -> UITapGestureRecognizer {
        let g = UITapGestureRecognizer()
        g.numberOfTouchesRequired = 2
        g.cancelsTouchesInView = false
        return g
    }
    func handleUIGestureRecognizerAction(_ recognizer: UITapGestureRecognizer, context: Context) {
        if recognizer.state == .ended { action() }
    }
}

/// Serbian Cyrillic → Latin (for the tappable "Српски" subtitle).
enum Serbian {
    static let map: [Character: String] = [
        "А": "A", "Б": "B", "В": "V", "Г": "G", "Д": "D", "Ђ": "Đ", "Е": "E", "Ж": "Ž", "З": "Z", "И": "I",
        "Ј": "J", "К": "K", "Л": "L", "Љ": "Lj", "М": "M", "Н": "N", "Њ": "Nj", "О": "O", "П": "P", "Р": "R",
        "С": "S", "Т": "T", "Ћ": "Ć", "У": "U", "Ф": "F", "Х": "H", "Ц": "C", "Ч": "Č", "Џ": "Dž", "Ш": "Š",
        "а": "a", "б": "b", "в": "v", "г": "g", "д": "d", "ђ": "đ", "е": "e", "ж": "ž", "з": "z", "и": "i",
        "ј": "j", "к": "k", "л": "l", "љ": "lj", "м": "m", "н": "n", "њ": "nj", "о": "o", "п": "p", "р": "r",
        "с": "s", "т": "t", "ћ": "ć", "у": "u", "ф": "f", "х": "h", "ц": "c", "ч": "č", "џ": "dž", "ш": "š",
    ]
    static func latin(_ s: String) -> String { s.map { map[$0] ?? String($0) }.joined() }
    static func isCyrillic(_ s: String) -> Bool { s.contains { map[$0] != nil } }
}

/// Share counts are private too: veiled alongside dollars.
struct SharesText: View {
    let value: Double
    @Environment(\.versoVeiled) private var veiled
    init(_ value: Double) { self.value = value }
    var body: some View {
        Text(veiled ? MoneyFormat.veil : MoneyFormat.shares(value))
            .monospacedDigit()
            .contentTransition(.numericText(value: value))
            .privacySensitive()
            .accessibilityLabel(veiled ? "hidden" : MoneyFormat.shares(value))
    }
}

/// A menu Picker set as a row: label on the left, choice on the right.
/// A bare menu Picker outside a Form drops its label and floats indented by its button padding.
struct PickerRow<P: View>: View {
    let label: String
    let picker: P
    init(_ label: String, @ViewBuilder picker: () -> P) { self.label = label; self.picker = picker() }
    var body: some View {
        HStack {
            Text(label).font(VersoFont.body(16)).foregroundStyle(Palette.ink)
            Spacer(minLength: 12)
            picker.labelsHidden().padding(.trailing, -10)
        }
    }
}

/// A date set like a ledger line: serif label, leader dots, the date in ledger figures. Tapping opens a
/// graphical calendar (or a time wheel) in a popover, so no stock SF date pill sits among the serif type.
struct PaperDateRow: View {
    let label: String
    @Binding var selection: Date
    var components: DatePickerComponents = .date
    var range: PartialRangeFrom<Date>? = nil
    var size: CGFloat = 16
    @State private var open = false

    var body: some View {
        Button { open = true } label: {
            LeaderRow {
                Text(label).font(VersoFont.body(size)).foregroundStyle(Palette.ink)
            } trailing: {
                Text(formatted).font(VersoFont.ledger(.body)).foregroundStyle(Palette.oxblood)
                    .contentTransition(.numericText())
            }
            .padding(.vertical, 6)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel(label)
        .accessibilityValue(formatted)
        .popover(isPresented: $open) {
            picker
                .tint(Palette.oxblood)
                .padding(12)
                .frame(minWidth: 320)
                .presentationCompactAdaptation(.popover)
                .presentationBackground(Palette.paper)
        }
    }

    @ViewBuilder var picker: some View {
        if components == .hourAndMinute {
            DatePicker(label, selection: $selection, displayedComponents: components)
                .datePickerStyle(.wheel).labelsHidden()
        } else if let range {
            DatePicker(label, selection: $selection, in: range, displayedComponents: components)
                .datePickerStyle(.graphical).labelsHidden()
        } else {
            DatePicker(label, selection: $selection, displayedComponents: components)
                .datePickerStyle(.graphical).labelsHidden()
        }
    }

    var formatted: String {
        switch components {
        case .hourAndMinute: selection.formatted(date: .omitted, time: .shortened)
        case .date: selection.formatted(.dateTime.day().month(.abbreviated).year())
        default: selection.formatted(.dateTime.day().month(.abbreviated).year().hour().minute())
        }
    }
}
