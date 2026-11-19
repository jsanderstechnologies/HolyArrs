using System;
using System.Collections.Generic;
using Jellyfin.Plugin.HolyArrs.Configuration;
using MediaBrowser.Common.Configuration;
using MediaBrowser.Common.Plugins;
using MediaBrowser.Model.Plugins;
using MediaBrowser.Model.Serialization;

namespace Jellyfin.Plugin.HolyArrs;

public class Plugin : BasePlugin<PluginConfiguration>, IHasWebPages
{
    public Plugin(IApplicationPaths applicationPaths, IXmlSerializer xmlSerializer)
        : base(applicationPaths, xmlSerializer)
    {
        Instance = this;
    }

    public static Plugin? Instance { get; private set; }

    public override string Name => "HolyArrs";

    public override Guid Id => Guid.Parse("a7b8c9d0-1234-4567-89ab-cdef01234567");

    public override string Description => "Browse, search, and request movies & shows directly from Jellyfin into HolyArrs (Torrentio + Premiumize.me .strm).";

    public IEnumerable<PluginPageInfo> GetPages()
    {
        var ns = GetType().Namespace;
        return new[]
        {
            new PluginPageInfo
            {
                Name = "holyarrs",
                EmbeddedResourcePath = $"{ns}.Configuration.configPage.html"
            },
            new PluginPageInfo
            {
                Name = "holyarrs-discover",
                EmbeddedResourcePath = $"{ns}.Web.discover.html",
                EnableInMainMenu = true,
                MenuSection = "server",
                MenuIcon = "travel_explore",
                DisplayName = "HolyArrs Discover"
            }
        };
    }
}
