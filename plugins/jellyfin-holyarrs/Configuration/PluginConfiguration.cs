using MediaBrowser.Model.Plugins;

namespace Jellyfin.Plugin.HolyArrs.Configuration;

public class PluginConfiguration : BasePluginConfiguration
{
    public string HolyArrsUrl { get; set; } = "http://holyarrs:6262";

    public string ApiKey { get; set; } = string.Empty;
}
