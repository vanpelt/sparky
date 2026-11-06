// Metal source, compiled at runtime (no offline Metal toolchain needed).
// Mirrors render.py: unified-omni + radtan V6 projection, angular seam feather,
// rolling-shutter fixed-point correction, optional preview LUT.
//
// Parameter buffer layout (floats):
//   lens i at [i*32 ..< i*32+32]:
//     0-8   body->lens rotation, row-major     9  xi
//     10-11 fx, fy (source px)                 12-13 cx, cy (source px)
//     14-18 k1..k5   19-22 p1..p4   23-26 s1..s4
//     27-28 x range of this lens in the texture  30-31 texture width, height
//   frame:
//     64-72 body<-camera rotation, row-major   73 focal (px, flat)   74 projection (0 flat, 1 equirect)
//     75 rolling-shutter readout (s, 0 = off)  76-78 body angular velocity (rad/s)
//     79 seam blend belt (deg)   80 look (0 log, 1 LUT)   81 LUT size
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

kernel void reframe(texture2d<float, access::write> out [[texture(0)]],
                    texture2d<float> luma0 [[texture(1)]], texture2d<float> chroma0 [[texture(2)]],
                    texture2d<float> luma1 [[texture(3)]], texture2d<float> chroma1 [[texture(4)]],
                    texture3d<float> lut [[texture(5)]],
                    constant float *P [[buffer(0)]],
                    uint2 gid [[thread_position_in_grid]]) {
    uint W = out.get_width(), H = out.get_height();
    if (gid.x >= W || gid.y >= H) return;

    float3 cam;
    if (P[74] < 0.5f) {
        float f = P[73];
        float u = (gid.x + 0.5f - W * 0.5f) / f, v = (gid.y + 0.5f - H * 0.5f) / f;
        cam = float3(1, -u, -v);  // forward +X, right -Y, down -Z
    } else {
        float lon = (gid.x + 0.5f) / W * 2 * M_PI_F - M_PI_F;
        float lat = M_PI_F * 0.5f - (gid.y + 0.5f) / H * M_PI_F;
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
            for (int it = 0; it < 2; it++) {  // source row -> capture time -> rotated ray
                float dt = (px.y / L[31] - 0.5f) * readout;
                px = projectLens(L, toLens(L, body - dt * cross(omega, body)), angle, valid);
            }
        }
        bool inside = valid && px.x >= L[27] && px.x <= L[28] - 1 && px.y >= 0 && px.y <= L[31] - 1;
        float w = inside ? clamp((90.0f - angle) / belt + 0.5f, 0.0f, 1.0f) : 0.0f;
        if (w > 0) {
            float2 uv = (px + 0.5f) / float2(L[30], L[31]);
            float3 rgb = i == 0 ? yuvToRGB(luma0.sample(linearClamp, uv).r, chroma0.sample(linearClamp, uv).rg)
                                : yuvToRGB(luma1.sample(linearClamp, uv).r, chroma1.sample(linearClamp, uv).rg);
            acc += rgb * w;
            wsum += w;
        }
    }
    float3 c = wsum > 0 ? saturate(acc / wsum) : float3(0);
    if (P[80] > 0.5f) {
        float n = P[81];
        float3 coord = (c * (n - 1) + 0.5f) / n;  // texel centres
        c = lut.sample(linearClamp, coord).rgb;
    }
    out.write(float4(c, 1), gid);
}
"""#
