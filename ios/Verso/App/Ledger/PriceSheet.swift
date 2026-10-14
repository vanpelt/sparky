import SwiftUI
import VersoCore

/// The price: source and age, a manual override (on/off), and Refresh.
struct PriceSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    @State private var override: Double?
    @State private var enabled = false
    @State private var refreshing = false

    var body: some View {
        let p = model.state.price
        let now = model.now()
        VStack(alignment: .leading, spacing: 14) {
            Text("THE PRICE").runningHeadStyle().padding(.top, 20)
            HStack(alignment: .lastTextBaseline) {
                Text(MoneyFormat.price(p.effective)).font(VersoFont.hero(54)).monospacedDigit()
                    .contentTransition(.numericText(value: p.effective ?? 0)).heroCap()
                Text(model.state.book.settings.ticker).runningHeadStyle(Palette.oxblood, size: 13)
                Spacer()
            }
            Text(sourceLine(p, now: now)).font(VersoFont.his(15))
                .foregroundStyle(p.isStale(now: now, calendar: model.calendar) ? Palette.rust : Palette.fadedInk)
            Hairline()
            OptionalMoneyRow(label: "Manual price", value: $override)
            PaperToggle(label: "Use the manual price everywhere", isOn: $enabled)
                .disabled(override == nil)
            HStack {
                QuietButton(refreshing ? "Refreshing…" : "Refresh", color: Palette.oxblood) {
                    refreshing = true
                    Task { await model.refreshPrice(force: true); refreshing = false }
                }
                Spacer()
                QuietButton("Done") { save(); dismiss() }
            }
            Spacer(minLength: 0)
        }
        // A book's measure, centred, however wide the sheet is presented.
        .frame(maxWidth: Measure.text, alignment: .leading)
        .padding(.horizontal, 24)
        .frame(maxWidth: .infinity)
        .onAppear { override = p.overridePrice; enabled = p.overrideEnabled }
        .onChange(of: enabled) { _, _ in save() }
        .onSubmit(save)
    }

    func sourceLine(_ p: PriceCache, now: Date) -> String {
        if p.isManual { return "Manual · set by you" }
        guard p.price != nil else { return "No quote yet. Set a price here, or refresh." }
        var s = "\(p.source ?? "Cached") · \(p.ageLabel(now: now))"
        if p.lastFailureAt != nil { s += " · last fetch failed" }
        return s
    }

    func save() {
        let o = override, e = enabled && override != nil
        guard o != model.state.price.overridePrice || e != model.state.price.overrideEnabled else { return }
        model.perform { try await $0.setOverride(o, enabled: e) }
    }
}
