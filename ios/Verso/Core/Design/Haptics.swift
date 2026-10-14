import SwiftUI
import CoreHaptics

/// Haptic vocabulary. SwiftUI moments use `.sensoryFeedback(...)` with these presets.
public enum Haptics {
    public static let log: SensoryFeedback = .selection
    public static let scale: SensoryFeedback = .selection
    public static let target: SensoryFeedback = .success
    public static let clauseMet: SensoryFeedback = .success
    public static let stampFirst: SensoryFeedback = .impact(weight: .medium)
    public static let stampSecond: SensoryFeedback = .impact(weight: .light)

    /// "Book closing": three transients (0.9 / 0.6 / 0.35, sharpness 0.3) spaced 0.18s.
    public static func bookClosingEvents(withRamp: Bool = false) -> [CHHapticEvent] {
        var events = [0.9, 0.6, 0.35].enumerated().map { i, intensity in
            CHHapticEvent(eventType: .hapticTransient, parameters: [
                CHHapticEventParameter(parameterID: .hapticIntensity, value: Float(intensity)),
                CHHapticEventParameter(parameterID: .hapticSharpness, value: 0.3),
            ], relativeTime: Double(i) * 0.18)
        }
        if withRamp {
            events.append(CHHapticEvent(eventType: .hapticContinuous, parameters: [
                CHHapticEventParameter(parameterID: .hapticIntensity, value: 0.2),
                CHHapticEventParameter(parameterID: .hapticSharpness, value: 0.2),
            ], relativeTime: 0.6, duration: 0.6))
        }
        return events
    }

    /// Plays the book-closing pattern (vest-day card), with a 0.6s ramp for Close Book One.
    @MainActor private static var engine: CHHapticEngine?

    @MainActor
    public static func playBookClosing(withRamp: Bool = false) {
        guard CHHapticEngine.capabilitiesForHardware().supportsHaptics else { return }
        do {
            let engine = try Self.engine ?? CHHapticEngine()
            Self.engine = engine
            try engine.start()
            var events = bookClosingEvents(withRamp: withRamp)
            var curves: [CHHapticParameterCurve] = []
            if withRamp {
                curves.append(CHHapticParameterCurve(parameterID: .hapticIntensityControl, controlPoints: [
                    .init(relativeTime: 0.6, value: 0.2), .init(relativeTime: 1.2, value: 1.0),
                ], relativeTime: 0))
            }
            let pattern = try CHHapticPattern(events: events, parameterCurves: curves)
            events.removeAll()
            let player = try engine.makePlayer(with: pattern)
            try player.start(atTime: 0)
        } catch {}
    }
}
