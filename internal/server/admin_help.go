package server

import "html/template"

// helpTab is the contextual Help panel WordPress shows above each admin screen
// (WP_Screen::add_help_tab() + set_help_sidebar()), printed by partials/layout.html.
type helpTab struct {
	Overview template.HTML // body of the "Overview" tab
	Doc      string        // "Documentation on …" link text
	DocURL   string
}

// screenHelp maps the adminpage global (body class, e.g. "edit-php") to its help tab. The texts
// follow WordPress's own help tabs, adapted to WordPress Go.
var screenHelp = map[string]helpTab{
	"edit-php": {`<p>This screen provides access to all of your posts. You can customize the display of this screen to suit your workflow.</p><p>You can narrow the list by post status using the text links above the posts list, refine it by date or category with the drop-downs and the Filter button, and search the list with the search box.</p><p>Hovering over a row in the posts list will display action links that allow you to manage your post: Edit, Quick Edit, Trash and View.</p>`,
		"Documentation on Managing Posts", "https://wordpress.org/documentation/article/posts-screen/"},
	"post-php": {`<p>The title field and the big Post Editing Area are fixed in place, but you can reposition all the other boxes using drag and drop. You can also minimize or expand them by clicking the title bar of each box.</p><p><strong>Title</strong> &mdash; Enter a title for your post. After you enter a title, you will see the permalink below, which you can edit.</p><p><strong>Post editor</strong> &mdash; Enter the text for your post. There are two modes of editing: Visual and Code.</p>`,
		"Documentation on Writing and Editing Posts", "https://wordpress.org/documentation/article/write-posts-classic-editor/"},
	"post-new-php": {`<p>The title field and the big Post Editing Area are fixed in place, but you can reposition all the other boxes using drag and drop.</p><p><strong>Title</strong> &mdash; Enter a title for your post. After you enter a title, you will see the permalink below, which you can edit.</p><p><strong>Publish</strong> &mdash; You can set the terms of publishing your post in the Publish box.</p>`,
		"Documentation on Writing and Editing Posts", "https://wordpress.org/documentation/article/write-posts-classic-editor/"},
	"edit-tags-php": {`<p>You can use categories and tags to group and sort your content. Use the form on the left to add a new term and the list on the right to edit, delete or view the content filed under each one.</p><p>When adding a new term, the slug is the URL-friendly version of the name. It is usually all lowercase and contains only letters, numbers, and hyphens.</p>`,
		"Documentation on Categories and Tags", "https://wordpress.org/documentation/article/posts-categories-screen/"},
	"term-php": {`<p>You can change the name, slug, parent and description of the term here. Changes take effect on the front end as soon as you click Update.</p>`,
		"Documentation on Categories and Tags", "https://wordpress.org/documentation/article/posts-categories-screen/"},
	"upload-php": {`<p>All the files you&#8217;ve uploaded are listed in the Media Library, with the most recent uploads listed first. You can use the Screen Options tab to customize the display of this screen.</p><p>You can narrow the list by file type/status or by date using the dropdown menus above the media table.</p><p>You can view your media in a simple visual grid or a list with columns. Switch between these views using the icons to the left above the media.</p>`,
		"Documentation on Media Library", "https://wordpress.org/documentation/article/media-library-screen/"},
	"media-new-php": {`<p>You can upload media files here without creating a post first. This allows you to upload files to use with posts and pages later and/or to get a web link for a particular file that you can share.</p><p>Drag and drop your files into the area below, or click Select Files to choose them from your computer.</p>`,
		"Documentation on Uploading Media Files", "https://wordpress.org/documentation/article/media-add-new-screen/"},
	"post-php-attachment": {`<p>This screen allows you to edit fields for metadata in a file within the media library.</p><p>For images only, you can click on Edit Image under the thumbnail to expand out an inline image editor with icons for cropping, rotating, or flipping the image as well as for undoing and redoing.</p>`,
		"Documentation on Edit Media", "https://wordpress.org/documentation/article/edit-media/"},
	"edit-comments-php": {`<p>You can manage comments made on your site similar to the way you manage posts and other content. This screen is customizable in the same ways as other management screens, and you can act on comments using the on-hover action links or the bulk actions.</p>`,
		"Documentation on Comments", "https://wordpress.org/documentation/article/comments-screen/"},
	"comment-php": {`<p>You can edit the information left in a comment if needed. This is often useful when you notice that a commenter has made a typographical error.</p><p>You can also moderate the comment from this screen using the Status box, where you can also change the timestamp of the comment.</p>`,
		"Documentation on Comments", "https://wordpress.org/documentation/article/comments-screen/"},
	"users-php": {`<p>This screen lists all the existing users for your site. Each user has one of five defined roles as set by the site admin: Site Administrator, Editor, Author, Contributor, or Subscriber.</p><p>Hover over any user to see action links: Edit, Delete and View.</p>`,
		"Documentation on Managing Users", "https://wordpress.org/documentation/article/users-screen/"},
	"user-new-php": {`<p>To add a new user to your site, fill in the form on this screen and click the Add User button at the bottom.</p><p>New users are automatically assigned a password, which they can change after logging in.</p>`,
		"Documentation on Adding New Users", "https://wordpress.org/documentation/article/users-add-new-screen/"},
	"profile-php": {`<p>Your profile contains information about you (your &#8220;account&#8221;) as well as some personal options related to using WordPress Go.</p><p>You can change your password, turn on keyboard shortcuts, change the color scheme of your admin screens, and turn off the Visual Editor, among other things.</p>`,
		"Documentation on User Profiles", "https://wordpress.org/documentation/article/users-your-profile-screen/"},
	"user-edit-php": {`<p>This screen shows the profile of another user. You can change their name, contact information, role and password.</p>`,
		"Documentation on User Profiles", "https://wordpress.org/documentation/article/users-your-profile-screen/"},
	"themes-php": {`<p>This screen is used for managing your installed themes. Aside from the default theme(s) included with WordPress Go, themes can be generated with Golementor&#8217;s Theme Builder.</p><p>From this screen you can activate a theme, preview it with Live Preview, or open the Customizer for the active theme. Hover or tap to see Activate and Live Preview buttons; click the theme to see its details.</p>`,
		"Documentation on Using Themes", "https://wordpress.org/documentation/article/work-with-themes/"},
	"theme-install-php": {`<p>You can add themes to your site here. Besides the themes compiled into WordPress Go (and the ones Golementor&#8217;s Theme Builder generates), a theme can be installed as a package: a zip of <code>theme.json</code>, <code>templates/</code> and <code>assets/</code> with no code in it. Click <strong>Upload Theme</strong>, choose the zip and click Install Now; the theme then appears on the Themes screen, where you can activate, customize or delete it.</p><p>A package with the slug of an installed package replaces it when its version is newer. Use the filter links or the search box to narrow the list.</p>`,
		"Documentation on Adding New Themes", "https://wordpress.org/documentation/article/appearance-themes-screen/"},
	"nav-menus-php": {`<p>This screen is used for managing your navigation menus.</p><p>Menus can be displayed in locations defined by your theme. You can add pages, posts, categories, tags and custom links to a menu and drag them into the order you prefer.</p><p>To create a new menu, click on the create a new menu link. Give the menu a name and click Create Menu, then add items from the column on the left.</p>`,
		"Documentation on Menus", "https://wordpress.org/documentation/article/appearance-menus-screen/"},
	"plugins-php": {`<p>Plugins extend and expand the functionality of WordPress Go. Once a plugin is installed, you may activate it or deactivate it here.</p><p>Native plugins are compiled into WordPress Go. Other plugins are signed WebAssembly packages that run in a sandbox with only the permissions they declare.</p>`,
		"Documentation on Managing Plugins", "https://wordpress.org/documentation/article/manage-plugins/"},
	"plugin-install-php": {`<p>Plugins hook into WordPress Go to extend its functionality with custom features. The Featured tab lists signed WebAssembly plugins you can install, Native Plugins lists the plugins built into WordPress Go, and Fan-made Demos shows concepts that are not available yet.</p><p>If you have a plugin package (.zip) you can upload it with the Upload Plugin button.</p>`,
		"Documentation on Installing Plugins", "https://wordpress.org/documentation/article/manage-plugins/#installing-plugins-1"},
	"options-general-php": {`<p>The fields on this screen determine some of the basics of your site setup.</p><p>Most themes show the site title at the top of every page, in the title bar of the browser, and as the identifying name for syndicated feeds. Many themes also show the tagline.</p><p>You must click the Save Changes button at the bottom of the screen for new settings to take effect.</p>`,
		"Documentation on General Settings", "https://wordpress.org/documentation/article/settings-general-screen/"},
	"options-writing-php": {`<p>You can submit content in several different ways; this screen holds the settings for all of them, including the default category and post format, and which editor Classic Editor uses.</p><p>You must click the Save Changes button at the bottom of the screen for new settings to take effect.</p>`,
		"Documentation on Writing Settings", "https://wordpress.org/documentation/article/settings-writing-screen/"},
	"options-reading-php": {`<p>This screen contains the settings that affect the display of your content.</p><p>You can choose what&#8217;s displayed on the homepage of your site. It can be posts in reverse chronological order (classic blog), or a fixed/static page.</p><p>You must click the Save Changes button at the bottom of the screen for new settings to take effect.</p>`,
		"Documentation on Reading Settings", "https://wordpress.org/documentation/article/settings-reading-screen/"},
	"options-discussion-php": {`<p>This screen provides many options for controlling the management and display of comments and links to your posts/pages.</p><p>You must click the Save Changes button at the bottom of the screen for new settings to take effect.</p>`,
		"Documentation on Discussion Settings", "https://wordpress.org/documentation/article/settings-discussion-screen/"},
	"options-media-php": {`<p>You can set maximum sizes for images inserted into your written content; you can also insert an image as Full Size.</p><p>You must click the Save Changes button at the bottom of the screen for new settings to take effect.</p>`,
		"Documentation on Media Settings", "https://wordpress.org/documentation/article/settings-media-screen/"},
	"options-permalink-php": {`<p>Permalinks are the permanent URLs to your individual pages and blog posts, as well as your category and tag archives.</p><p>This screen allows you to choose your permalink structure. You can choose from common settings or create custom URL structures.</p><p>You must click the Save Changes button at the bottom of the screen for new settings to take effect.</p>`,
		"Documentation on Permalinks Settings", "https://wordpress.org/documentation/article/settings-permalinks-screen/"},
	"options-privacy-php": {`<p>The Privacy screen lets you either build a new privacy-policy page or choose one you already have to show.</p><p>This screen includes suggestions to help you write your own privacy policy.</p>`,
		"Documentation on Privacy Settings", "https://wordpress.org/documentation/article/settings-privacy-screen/"},
	"tools-php": {`<p>This screen lists the tools that are available on your site, such as the category and tag converter and the WordPress Go importer.</p>`,
		"Documentation on Tools", "https://wordpress.org/documentation/article/tools-screen/"},
	"import-php": {`<p>This screen lists links to plugins to import data from blogging/content management platforms. WordPress Go imports WordPress eXtended RSS (WXR) export files directly.</p>`,
		"Documentation on Import", "https://wordpress.org/documentation/article/tools-import-screen/"},
	"export-php": {`<p>You can export a file of your site&#8217;s content in order to import it into another installation or platform. The export file will be an XML file format called WXR. Posts, pages, comments, custom fields, categories, and tags can be included.</p>`,
		"Documentation on Export", "https://wordpress.org/documentation/article/tools-export-screen/"},
	"site-health-php": {`<p>This screen allows you to obtain a health diagnosis of your site, and displays an overall rating of the status of your installation.</p><p>In the Status tab, you can see critical information about your WordPress Go configuration, along with anything else that requires your attention.</p><p>In the Info tab, you will find all the details about the configuration of your site, which you can copy to the clipboard.</p>`,
		"Documentation on Site Health tool", "https://wordpress.org/documentation/article/site-health-screen/"},
	"export-personal-data-php": {`<p>This screen is where you manage requests for an export of personal data.</p><p>Privacy Laws around the world require businesses and online services to provide an export of some of the data they collect about an individual, and to deliver that export on request.</p>`,
		"Documentation on Export Personal Data", "https://wordpress.org/documentation/article/tools-export-personal-data-screen/"},
	"erase-personal-data-php": {`<p>This screen is where you manage requests to erase personal data.</p><p>Privacy Laws around the world require businesses and online services to delete, anonymize, or forget the data they collect about an individual.</p>`,
		"Documentation on Erase Personal Data", "https://wordpress.org/documentation/article/tools-erase-personal-data-screen/"},
	"theme-editor-php": {`<p>You can browse the files that make up your themes here. WordPress Go compiles theme files into the binary, so the file editor is read-only.</p><p>Begin by choosing a theme in the dropdown menu and clicking the Select button. A list then appears of the theme&#8217;s template files. Clicking once on any file name causes the file to appear in the large viewer.</p>`,
		"Documentation on Theme Development", "https://developer.wordpress.org/themes/"},
	"plugin-editor-php": {`<p>You can browse the files inside your installed plugin packages here. Plugins are signed WebAssembly packages, so the file editor is read-only.</p><p>Choose a plugin to view from the dropdown menu and click the Select button. Click once on any file name in the file tree on the right to view it.</p>`,
		"Documentation on Editing Plugins", "https://developer.wordpress.org/plugins/"},
	"update-core-php": {`<p>On this screen, you can check which version of WordPress Go you are running. WordPress Go is a single compiled binary: updating means replacing the binary and restarting it.</p>`,
		"Documentation on Updating WordPress", "https://wordpress.org/documentation/article/dashboard-updates-screen/"},
}

// helpFor returns the help tab for an admin page, or nil when the screen has none (or prints its own).
func helpFor(adminPage, typeNow string) *helpTab {
	key := adminPage
	if adminPage == "post-php" && typeNow == "attachment" {
		key = "post-php-attachment"
	}
	h, ok := screenHelp[key]
	if !ok {
		return nil
	}
	if typeNow == "page" {
		switch adminPage {
		case "edit-php":
			h = helpTab{`<p>Pages are similar to posts in that they have a title, body text, and associated metadata, but they are different in that they are not part of the chronological blog stream, kind of like permanent posts.</p><p>Managing pages is very similar to managing posts, and the screens can be customized in the same way.</p>`,
				"Documentation on Managing Pages", "https://wordpress.org/documentation/article/pages-screen/"}
		case "post-php", "post-new-php":
			h.Doc, h.DocURL = "Documentation on Adding New Pages", "https://wordpress.org/documentation/article/pages-add-new-screen/"
		}
	}
	return &h
}
