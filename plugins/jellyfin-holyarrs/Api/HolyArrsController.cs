using System;
using System.Net.Http;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using Microsoft.AspNetCore.Authorization;
using Microsoft.AspNetCore.Http;
using Microsoft.AspNetCore.Mvc;

namespace Jellyfin.Plugin.HolyArrs.Api;

[ApiController]
[Route("HolyArrs")]
[Authorize]
public class HolyArrsController : ControllerBase
{
    private static readonly HttpClient Http = new() { Timeout = TimeSpan.FromSeconds(30) };

    [HttpGet("Ping")]
    public async Task<IActionResult> Ping(CancellationToken ct)
    {
        var cfg = Plugin.Instance?.Configuration;
        if (cfg == null || string.IsNullOrWhiteSpace(cfg.HolyArrsUrl))
        {
            return BadRequest(new { error = "HolyArrs URL is not configured." });
        }

        using var req = BuildRequest(HttpMethod.Get, $"{TrimUrl(cfg.HolyArrsUrl)}/api/v1/metadata/health", cfg.ApiKey);
        using var resp = await Http.SendAsync(req, ct).ConfigureAwait(false);
        var body = await resp.Content.ReadAsStringAsync(ct).ConfigureAwait(false);
        return StatusCode((int)resp.StatusCode, body);
    }

    [HttpGet("Search")]
    public async Task<IActionResult> Search([FromQuery] string q, CancellationToken ct)
    {
        if (string.IsNullOrWhiteSpace(q))
        {
            return Ok(new { items = Array.Empty<object>() });
        }

        var cfg = Plugin.Instance?.Configuration;
        if (cfg == null || string.IsNullOrWhiteSpace(cfg.HolyArrsUrl))
        {
            return BadRequest(new { error = "HolyArrs URL is not configured." });
        }

        var target = $"{TrimUrl(cfg.HolyArrsUrl)}/api/v1/metadata/search?q={Uri.EscapeDataString(q)}";
        using var req = BuildRequest(HttpMethod.Get, target, cfg.ApiKey);
        using var resp = await Http.SendAsync(req, ct).ConfigureAwait(false);
        var body = await resp.Content.ReadAsStringAsync(ct).ConfigureAwait(false);
        return Content(body, "application/json");
    }

    [HttpPost("Request/{kind}/{tmdbId}")]
    public async Task<IActionResult> RequestTitle([FromRoute] string kind, [FromRoute] string tmdbId, CancellationToken ct)
    {
        if (kind != "movie" && kind != "show")
        {
            return BadRequest(new { error = "kind must be movie or show" });
        }

        var cfg = Plugin.Instance?.Configuration;
        if (cfg == null || string.IsNullOrWhiteSpace(cfg.HolyArrsUrl))
        {
            return BadRequest(new { error = "HolyArrs URL is not configured." });
        }

        var target = $"{TrimUrl(cfg.HolyArrsUrl)}/api/v1/metadata/{kind}/{Uri.EscapeDataString(tmdbId)}/follow";
        using var req = BuildRequest(HttpMethod.Post, target, cfg.ApiKey);
        req.Content = new StringContent("{}", Encoding.UTF8, "application/json");
        using var resp = await Http.SendAsync(req, ct).ConfigureAwait(false);
        var body = await resp.Content.ReadAsStringAsync(ct).ConfigureAwait(false);
        return StatusCode((int)resp.StatusCode, body);
    }

    private static string TrimUrl(string url) => url.Trim().TrimEnd('/');

    private static HttpRequestMessage BuildRequest(HttpMethod method, string url, string apiKey)
    {
        var req = new HttpRequestMessage(method, url);
        if (!string.IsNullOrWhiteSpace(apiKey))
        {
            req.Headers.TryAddWithoutValidation("X-Api-Key", apiKey.Trim());
        }
        return req;
    }
}
