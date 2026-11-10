// Metal source, compiled at runtime (no offline Metal toolchain needed).
// Mirrors render.py: unified-omni + radtan V6 projection, angular seam feather,
// rolling-shutter fixed-point correction, optional preview LUT.
//
// Parameter buffer layout (floats):
//   lens i at [i*32 ..< i*32+32]:
//     0-8   body->lens rotation, row-major     9  xi
//     10-11 fx, fy (source px)                 12-13 cx, cy (source px)
//     14-18 k1..k5   19-22 p1..p4   23-26 s1..s4
//     27-28 x range of this lens in the texture  29 sample scale (10-bit fix)  30-31 texture width, height
//   frame:
//     64-72 body<-camera rotation, row-major   73 focal (px, flat)   74 projection (0 flat, 1 equirect)
//     75 rolling-shutter readout (s, 0 = off)  76-78 body angular velocity (rad/s)
//     79 seam blend belt (deg)   80 look (0 log, 1 LUT)   81 LUT size   82 projection distortion (0...1)
//     83 rolling-shutter row-table size (0 = linear)   96...: row poses, 9 floats each, row-major
let reframeShaderSource = #"""
#include <metal_stdlib>
using namespace metal;

constexpr sampler linearClamp(coord::normalized, address::clamp_to_edge, filter::linear);

inline float3 yuvToRGB(float y, float2 c) {
    // BT.709 limited range; normalized values work for both 8-bit and P010 sources.
    float Y = (y - 0.06256) * 1.16781;
    float Cb = (c.x - 0.50049) * 1.14174;
    float Cr = (c.y - 0.50049) * 1.14174;
    return float3(Y + 1.5748 * Cr, Y - 0.18733 * Cb - 0.46812 * Cr, Y + 1.8556 * Cb);
}

inline float3 toLens(constant float *L, float3 b) {
    return float3(L[0] * b.x + L[1] * b.y + L[2] * b.z,
                  L[3] * b.x + L[4] * b.y + L[5] * b.z,
                  L[6] * b.x + L[7] * b.y + L[8] * b.z);
}

inline float2 projectLens(constant float *L, float3 local, thread float &angle, thread bool &valid) {
    float n = length(local);
    angle = acos(clamp(local.z / n, -1.0f, 1.0f)) * 57.29577951f;
    valid = angle < 100.0f;  // the omni model folds back beyond the front hemisphere
    if (!valid) { local = float3(0, 0, 1); n = 1; }
    float den = local.z + L[9] * n;
    float mx = local.x / den, my = local.y / den;
    float x2 = mx * mx, y2 = my * my, xy = mx * my, r2 = x2 + y2, r4 = r2 * r2;
    float radial = 1 + r2 * (L[14] + r2 * (L[15] + r2 * (L[16] + r2 * (L[17] + r2 * L[18]))));
    float p1 = L[19], p2 = L[20], p3 = L[21], p4 = L[22];
    float dx = mx * radial + (p1 + r2 * p3) * (r2 + 2 * x2) + 2 * (p2 + r2 * p4) * xy + L[23] * r2 + L[25] * r4;
    float dy = my * radial + (p2 + r2 * p4) * (r2 + 2 * y2) + 2 * (p1 + r2 * p3) * xy + L[24] * r2 + L[26] * r4;
    return float2(L[12] + dx * L[10], L[13] + dy * L[11]);
}

/// Ray through output pixel `pos` -> blended RGB from both lenses (camera encoding, 0..1).
inline float3 shade(float2 pos, float W, float H, texture2d<float> luma0, texture2d<float> chroma0,
                    texture2d<float> luma1, texture2d<float> chroma1, constant float *P) {
    float3 cam;
    if (P[74] < 0.5f) {
        // Inverse unified camera model: d = 0 rectilinear ... d = 1 stereographic.
        float f = P[73], d = P[82];
        float u = (pos.x - W * 0.5f) / f, v = (pos.y - H * 0.5f) / f;
        float r2 = u * u + v * v;
        float k = (d + sqrt(1 + (1 - d * d) * r2)) / (r2 + 1);
        cam = float3(k - d, -k * u, -k * v);  // forward +X, right -Y, down -Z
    } else {
        float lon = pos.x / W * 2 * M_PI_F - M_PI_F;
        float lat = M_PI_F * 0.5f - pos.y / H * M_PI_F;
        cam = float3(cos(lat) * cos(lon), -cos(lat) * sin(lon), sin(lat));
    }
    float3 body = float3(P[64] * cam.x + P[65] * cam.y + P[66] * cam.z,
                         P[67] * cam.x + P[68] * cam.y + P[69] * cam.z,
                         P[70] * cam.x + P[71] * cam.y + P[72] * cam.z);
    float readout = P[75];
    float3 omega = float3(P[76], P[77], P[78]);
    float belt = P[79];

    float3 acc = 0;
    float wsum = 0;
    for (int i = 0; i < 2; i++) {
        constant float *L = P + i * 32;
        float angle; bool valid;
        float2 px = projectLens(L, toLens(L, body), angle, valid);
        if (readout > 0) {
            int rows = int(P[83]);
            for (int it = 0; it < 2; it++) {  // source row -> capture time -> rotated ray
                float3 b;
                if (rows > 1) {  // exact: interpolate the per-row pose table
                    float fi = clamp(px.y / L[31], 0.0f, 1.0f) * (rows - 1);
                    int i0 = min(int(fi), rows - 2);
                    float a = fi - i0;
                    constant float *R0 = P + 96 + i0 * 9, *R1 = R0 + 9;
                    float3 b0 = float3(dot(float3(R0[0], R0[1], R0[2]), body), dot(float3(R0[3], R0[4], R0[5]), body),
                                       dot(float3(R0[6], R0[7], R0[8]), body));
                    float3 b1 = float3(dot(float3(R1[0], R1[1], R1[2]), body), dot(float3(R1[3], R1[4], R1[5]), body),
                                       dot(float3(R1[6], R1[7], R1[8]), body));
                    b = mix(b0, b1, a);
                } else {  // linear: constant angular velocity across the frame
                    float dt = (px.y / L[31] - 0.5f) * readout;
                    b = body - dt * cross(omega, body);
                }
                px = projectLens(L, toLens(L, b), angle, valid);
            }
        }
        bool inside = valid && px.x >= L[27] && px.x <= L[28] - 1 && px.y >= 0 && px.y <= L[31] - 1;
        float w = inside ? clamp((90.0f - angle) / belt + 0.5f, 0.0f, 1.0f) : 0.0f;
        if (w > 0) {
            float2 uv = (px + 0.5f) / float2(L[30], L[31]);
            float k = L[29];  // P010 stores v10 << 6; rescale so 1023 -> 1.0
            float3 rgb = i == 0
                ? yuvToRGB(luma0.sample(linearClamp, uv).r * k, chroma0.sample(linearClamp, uv).rg * k)
                : yuvToRGB(luma1.sample(linearClamp, uv).r * k, chroma1.sample(linearClamp, uv).rg * k);
            acc += rgb * w;
            wsum += w;
        }
    }
    return wsum > 0 ? saturate(acc / wsum) : float3(0);
}

inline float3 applyLUT(float3 c, texture3d<float> lut, constant float *P) {
    if (P[80] < 0.5f) return c;
    float n = P[81];
    return lut.sample(linearClamp, (c * (n - 1) + 0.5f) / n).rgb;  // texel centres
}

/// Preview / stills: RGB(A) output.
kernel void reframe(texture2d<float, access::write> out [[texture(0)]],
                    texture2d<float> luma0 [[texture(1)]], texture2d<float> chroma0 [[texture(2)]],
                    texture2d<float> luma1 [[texture(3)]], texture2d<float> chroma1 [[texture(4)]],
                    texture3d<float> lut [[texture(5)]],
                    constant float *P [[buffer(0)]],
                    uint2 gid [[thread_position_in_grid]]) {
    uint W = out.get_width(), H = out.get_height();
    if (gid.x >= W || gid.y >= H) return;
    float3 c = shade(float2(gid) + 0.5f, W, H, luma0, chroma0, luma1, chroma1, P);
    out.write(float4(applyLUT(c, lut, P), 1), gid);
}

/// Export: 10-bit 4:2:2 Y'CbCr (BT.709, video range) straight into an 'x422' pixel buffer.
/// One thread per horizontal pixel pair; chroma is the average of the pair.
kernel void reframeYUV422(texture2d<float, access::write> outY [[texture(0)]],
                          texture2d<float> luma0 [[texture(1)]], texture2d<float> chroma0 [[texture(2)]],
                          texture2d<float> luma1 [[texture(3)]], texture2d<float> chroma1 [[texture(4)]],
                          texture3d<float> lut [[texture(5)]],
                          texture2d<float, access::write> outC [[texture(6)]],
                          constant float *P [[buffer(0)]],
                          uint2 gid [[thread_position_in_grid]]) {
    uint W = outY.get_width(), H = outY.get_height();
    if (gid.x * 2 >= W || gid.y >= H) return;
    const float q = 64.0f / 65535.0f * 1023.0f;  // normalized 10-bit value -> 16-bit container
    float2 cbcr = 0;
    for (uint k = 0; k < 2; k++) {
        uint2 p = uint2(gid.x * 2 + k, gid.y);
        float3 c = applyLUT(shade(float2(p) + 0.5f, W, H, luma0, chroma0, luma1, chroma1, P), lut, P);
        float y = dot(c, float3(0.2126f, 0.7152f, 0.0722f));
        outY.write(float4((y * 876.0f + 64.0f) / 1023.0f * q, 0, 0, 0), p);
        cbcr += float2((c.b - y) / 1.8556f, (c.r - y) / 1.5748f) * 0.5f;
    }
    outC.write(float4((cbcr * 896.0f + 512.0f) / 1023.0f * q, 0, 0), gid);
}
"""#
